# NOFX 部署文档

从零部署 NOFX 模拟交易机器人：OKX 行情 + AI 决策 + 本地模拟盘 + Telegram 双向控制。

## 1. 环境要求

| 依赖 | 说明 |
| :--- | :--- |
| Go 1.22+ | 仅构建时需要，运行时为单个静态二进制 |
| 网络 | 能访问 OKX（`www.okx.com`）和 Telegram Bot API；国内服务器需配置代理 |
| 磁盘 | 几十 MB 即可（SQLite 账本 + 日志），无外部数据库 |

## 2. 构建与首次启动

```bash
git clone git@github.com:GIN-zhao/nofx.git && cd nofx
git checkout minimal-paper

cp .env.example .env   # 按下节填写
go build -o nofx .

./nofx                 # 前台运行；生产环境用 systemd / nohup，见 §5
```

启动成功的标志（日志）：

```
✅ Configuration loaded
✅ Database initialized
✅ Paper trader ready: initial balance 1000.00 USDT
✅ AI client ready: provider=claude model=glm-5.3
✅ Telegram 指令监听已启动 (发送 /help 查看可用指令)
🤖 [Trader paper] AI trading loop started (interval: 5m0s)
```

## 3. 配置（.env）

### 3.1 AI 决策模型（必填）

两种协议二选一：

**方式 A：Anthropic 协议（智谱 GLM 订阅套餐走这个）**

```bash
AI_PROVIDER=claude
AI_BASE_URL=https://open.bigmodel.cn/api/anthropic/v1
AI_MODEL=glm-5.3
AI_API_KEY=<智谱 API Key>
AI_MAX_TOKENS=8000     # glm-5.3 是思考模型，建议 ≥8000
```

> 注意：智谱**订阅套餐 key** 只能用 `/api/anthropic` 端点（Anthropic 协议）；
> **按量付费**余额走的是 `/api/paas/v4`（OpenAI 协议）。走错端点会报
> `429 code=1113 余额不足`。判断方法见 §7 常见问题。

**方式 B：OpenAI 兼容协议（deepseek / openai / qwen / kimi / grok 等）**

```bash
AI_PROVIDER=deepseek          # deepseek/openai/qwen/gemini/grok/kimi/minimax
AI_API_KEY=<key>
AI_MODEL=deepseek-chat        # 留空用 provider 默认模型
AI_BASE_URL=                  # 留空用 provider 默认地址
AI_MAX_TOKENS=4000
```

### 3.2 Telegram 机器人（必填才能推送和指令）

```bash
TELEGRAM_BOT_TOKEN=<@BotFather 创建 bot 后发放的 token>
TELEGRAM_CHAT_ID=<你的数字 chat id>
```

获取 chat_id：给 bot 发一条消息，然后浏览器打开
`https://api.telegram.org/bot<TOKEN>/getUpdates`，在返回 JSON 里找 `"chat":{"id":...}`。

> 安全机制：bot 只响应 `TELEGRAM_CHAT_ID` 对应会话，陌生人消息直接忽略。

### 3.3 模拟盘与调度

| 变量 | 说明 | 默认 |
| :--- | :--- | :--- |
| `DB_PATH` | SQLite 账本路径 | `data/data.db` |
| `INITIAL_BALANCE` | 初始本金 USDT（仅首次建账生效，之后改它不影响已有账本） | `1000` |
| `SCAN_INTERVAL` | 决策周期秒数（最小 30） | `300` |
| `OKX_BASE_URL` | OKX 公开 REST 地址 | `https://www.okx.com` |

### 3.4 代理（国内服务器必看）

```bash
PROXY_URL=http://127.0.0.1:7897   # Clash/V2Ray 混合端口
NO_PROXY=open.bigmodel.cn         # 这些域名直连，不走代理（支持域名后缀、逗号分隔、* 全直连）
```

流量分配规则：

- **走代理**：OKX 行情、Telegram 推送与指令轮询（国内被墙）
- **直连**：`NO_PROXY` 列表中的域名 + 未配代理时的全部流量

> 常见坑：国内 AI 服务（如 open.bigmodel.cn）必须加入 `NO_PROXY`。
> Clash 部分节点连国内域名会被直接掐断（表现为请求 EOF），详见 §7。

## 4. Telegram 使用

| 指令 | 作用 |
| :--- | :--- |
| `/status` | 账户总览（权益/盈亏/胜率/回撤） |
| `/positions` | 持仓明细（开仓价/现价/浮动/SL/TP/爆仓价） |
| `/trades` | 最近 10 笔平仓记录 |
| `/pause` `/resume` | 暂停/恢复开新仓（已有持仓继续管理） |
| `/help` | 帮助 |

直接发自然语言，AI 会调用工具执行：

- 查行情：「现在 BTC 什么行情」「DOGE 4小时线怎么看」（实时调 OKX 接口）
- 改策略：「把最低信心度改成 70」「候选加 DOGE，最大持仓改 5」（立即持久化并在下个周期生效）
- **执行交易**：「平掉 BTC」「ETH 方向错了，止损离场」「把 BTC 止损上移到 78000」
  ——真实成交并推送记录，只有配置的 chat 能触发

## 5. 进程管理

### systemd（Linux 推荐）

```ini
# /etc/systemd/system/nofx.service
[Unit]
Description=NOFX paper trading bot
After=network-online.target

[Service]
Type=simple
User=nofx
WorkingDirectory=/opt/nofx
ExecStart=/opt/nofx/nofx
Restart=always
RestartSec=10
# 账本优雅落盘需要，收到 SIGTERM 后等待周期结束
KillSignal=SIGTERM
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now nofx
journalctl -u nofx -f
```

### nohup（临时跑）

```bash
nohup ./nofx > nofx.log 2>&1 &        # 必须在项目根目录启动（.env 相对读取）
tail -f nofx.log
```

### 停止

```bash
kill <pid>     # SIGTERM：完成当前决策周期后退出，账本始终一致
```

## 6. 数据与升级

- **账本**：`data/data.db`（持仓/订单/成交/决策记录/权益曲线）。删除即重置账户，
  备份直接 `cp` 即可（建议先 `kill` 停机再拷贝）
- **升级**：

```bash
git pull && go build -o nofx-new .
kill <旧pid> && wait            # 优雅退出
mv nofx-new nofx && nohup ./nofx > nofx.log 2>&1 &
```

重启后自动恢复：账本、SL/TP、策略配置（SQLite）全部延续；Telegram 未处理的消息
会在服务端保留 24 小时，重启后自动补收。

## 7. 常见问题

| 现象 | 原因与处理 |
| :--- | :--- |
| `429 code=1113 余额不足或无可用资源包` | 订阅 key 走了按量付费端点（或反之）。订阅套餐 → `AI_PROVIDER=claude` + `AI_BASE_URL=https://open.bigmodel.cn/api/anthropic/v1`；按量付费 → OpenAI 兼容协议 + `/api/paas/v4` |
| AI 请求报 `EOF` / `context deadline exceeded` | 代理节点连不上该域名。国内 AI 服务加入 `NO_PROXY` 直连；若代理域名本身不通，换 Clash 节点 |
| `telegram getUpdates failed: ... unexpected EOF` | 代理长连接偶发抖动，程序每 3 秒自动重试，偶发无需处理；持续出现按上一条处理 |
| Telegram 发消息不回复 | 确认发消息的账号 chat_id 与 `TELEGRAM_CHAT_ID` 一致；确认进程存活（`pgrep nofx`）；重启后会补收 24h 内消息 |
| 决策周期连续失败 | 连续 3 次失败进入安全模式并推送告警；恢复后自动退出安全模式。期间 SL/TP/爆仓检查照常执行，持仓仍受保护 |
| 开仓被拒 | 见日志：最低仓位、最大持仓数、仓位占比上限、SL/TP 方向校验均为风控硬约束，属预期行为 |
| 日志出现乱码/截断 | 多为多进程写同一日志文件所致，重启时换一个日志文件名 |
