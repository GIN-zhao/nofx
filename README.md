# NOFX — OKX 行情 + AI 决策 + 本地模拟交易 + Telegram 推送

精简版 NOFX：只用 OKX 公开行情（无需交易所 API key），由大模型做交易决策，在本地 SQLite 账本上模拟交易，并通过 Telegram bot 推送每笔交易的详细信息。

已移除：钱包/交易所实盘连接、Web 前端、多交易所适配、网格策略等冗余模块。

## 功能

- **OKX 实时行情**：USDT 永续合约 K 线（多周期）、EMA/MACD/RSI/ATR/BOLL、持仓量、资金费率，全部来自 OKX v5 公开 REST。
- **AI 决策**：将行情 + 账户 + 持仓 + 历史交易统计组装进 prompt，由配置的 LLM 输出结构化决策（开多/开空/平仓/持有，含止损、止盈、杠杆、仓位大小、信心度）。
- **本地模拟交易**：SQLite 账本（trader/positions/orders/equity 表），市价即时成交，0.05% taker 手续费，强制止损/止盈，按 `1/杠杆` 估算爆仓价，重启后账本自动恢复。
- **Telegram 推送**：开仓/平仓消息包含策略、币种现价与 1h/4h 涨跌（市场走向）、本金（保证金）、杠杆、仓位价值、止损止盈、信心度和 AI 分析；触发止损/止盈/爆仓也会推送。连续 3 次周期失败自动进入安全模式并推送告警。
- **Telegram 双向指令**：直接给 bot 发消息即可查询和控制（仅限配置的 chat）：`/status` 账户总览、`/positions` 持仓明细、`/trades` 最近平仓、`/pause` 暂停开仓、`/resume` 恢复开仓、`/help` 帮助；发任意问题 AI 会结合当前账户与持仓状态回答。
- **风控兜底**：最大持仓数、BTC/ETH 与山寨币仓位占比上限、最小仓位金额、SL/TP 价格合法性校验（方向错误立即拒绝开仓）。

## 快速开始

```bash
cp .env.example .env   # 填入 AI_API_KEY（和 Telegram 配置）
go build -o nofx .
./nofx
```

默认候选币种 BTC / ETH / SOL，每 300 秒一个决策周期（`SCAN_INTERVAL` 可调，最小 30s），初始本金 1000 USDT（`INITIAL_BALANCE`）。

## 环境变量

| 变量 | 说明 | 默认 |
| :--- | :--- | :--- |
| `DB_PATH` | SQLite 文件路径 | `data/data.db` |
| `AI_PROVIDER` | deepseek / openai / claude / qwen / gemini / grok / kimi / minimax | `deepseek` |
| `AI_MODEL` | 模型名，留空用 provider 默认 | 空 |
| `AI_API_KEY` | AI key（也可用 `DEEPSEEK_API_KEY` 等常规变量） | 必填 |
| `TELEGRAM_BOT_TOKEN` | @BotFather 创建 bot 获得 | 空（禁用推送） |
| `TELEGRAM_CHAT_ID` | 目标 chat id | 空（禁用推送） |
| `INITIAL_BALANCE` | 初始本金 USDT（仅首次建账生效） | `1000` |
| `SCAN_INTERVAL` | 决策周期秒数 | `300` |
| `OKX_BASE_URL` | OKX 公开 REST 地址 | `https://www.okx.com` |

## 交易周期流程

1. 检查止损/止盈/爆仓（市价触发，平仓并推送）。
2. 组装上下文：账户权益、持仓、最近平仓记录、胜率/盈亏比/最大回撤。
3. 拉取候选币种多周期 K 线与指标，调用 LLM 输出决策。
4. 持久化决策与权益快照，然后按「先平仓后开仓、高信心优先」执行。
5. 节流保护：最小持仓 90 分钟（价格亏损 ≤ -3% 或盈利 ≥ +8% 可提前平），噪声区间（90 分钟~3 小时、-2%~+3%）不平，平仓后 4 小时内禁止同币种再入场。

## 目录结构

```
main.go               启动装配
config/               进程配置（.env）
market/               OKX 公开行情 + 指标计算
kernel/               AI 决策引擎（prompt 组装、决策解析、风控参数）
trader/               自动交易循环（周期调度、节流、风控、Telegram 消息）
trader/paper/         本地模拟账本 broker
trader/types/         broker 接口
store/                GORM + SQLite 持久层
mcp/                  多 provider LLM 客户端
notify/               Telegram bot 推送
```

## 免责声明

本项目仅用于模拟交易研究，不构成投资建议。模拟结果不代表实盘表现。
