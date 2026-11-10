# NOFX 实盘交易说明（TRADING_MODE=live）

`live-trading` 分支新增：机器人可直连 OKX 真实账户（或 OKX 模拟盘 demo），替代本地
模拟账本执行交易。行情、AI 决策、Telegram 推送与指令、风控节流逻辑全部不变，只换了
执行交易的 broker。配置方法见 [DEPLOY.md](DEPLOY.md) §3.4。

> ⚠️ 实盘涉及真实资金。请先 `OKX_DEMO=1` 在 OKX 模拟盘完整验证一个交易日以上，
> 确认开仓/止损/止盈/平仓推送都符合预期后再切 `OKX_DEMO=0`。

## 1. 三种运行模式对比

| | paper（默认） | live + OKX_DEMO=1 | live + OKX_DEMO=0 |
| :--- | :--- | :--- | :--- |
| 交易场所 | 本地 SQLite 账本 | OKX 模拟盘 | **OKX 实盘** |
| 资金 | 虚拟 USDT | 交易所虚拟资金 | **真实 USDT** |
| 需要 OKX API key | 否 | 是 | 是 |
| SL/TP 执行方 | 本地周期检查（90s 一次） | 交易所策略委托（实时触发） | 交易所策略委托（实时触发） |
| 机器人宕机时持仓 | 账本重启恢复，SL/TP 恢复 | 交易所侧继续保护 | 交易所侧继续保护 |
| 启动日志标识 | `Paper trader ready` | `🎮 OKX 模拟盘` | `🚨 OKX 实盘` |

三种模式下 AI 决策周期、Telegram 双向指令（平仓/改止损）、风控（最大持仓数、
仓位占比、最小仓位、节流）行为完全一致。

## 2. 架构：可插拔 broker

所有交易动作通过 `trader/types.Trader` 接口（trader/types/interface.go），
`main.go` 按 `TRADING_MODE` 选择实现：

```
types.Trader 接口
├── trader/paper    本地模拟账本（默认）
└── trader/okxlive  OKX v5 私有 REST（本分支新增）
```

`trader/okxlive` 三个文件的职责：

| 文件 | 内容 |
| :--- | :--- |
| `trader.go` | HMAC-SHA256 签名客户端、账户校验与双向持仓切换、合约参数（ctVal/lotSz/minSz）缓存 |
| `orders.go` | 市价开/平仓（张数换算）、SL/TP 策略委托（挂单/改单/撤单）、杠杆设置 |
| `account.go` | 余额/持仓/最新价（映射为 paper 兼容格式）、服务端平仓检测（CheckStops） |

## 3. OKX 账户要求

- **合约类型**：USDT 永续（SWAP），其他品种（币本位、交割、现货）不下单。
- **持仓模式**：双向持仓 (long_short_mode)。启动时若检测到单向持仓会自动尝试切换，
  账户有持仓或挂单时切换会失败——先在 OKX 网页平掉，或手动切换后重启。
- **保证金模式**：默认全仓（`OKX_ISOLATED=0`），可切逐仓。
- **API key 权限**：只需「交易」，不要勾选「提币」；建议绑定服务器 IP 白名单。

## 4. 交易行为细节

### 下单

- AI 决策给出的是**币的数量**（如 0.05 BTC），broker 按 `张数 = 数量 / ctVal`
  换算并向下取整到 lotSz；低于最小张数直接报错（不会部分成交）。
- 开仓前自动撤掉该币该方向的旧策略委托（防止残留保护单干扰）、设置杠杆、然后市价单。

### 止损 / 止盈（挂交易所侧）

- 以 OKX 策略委托（algo order）挂在交易所：单腿 conditional，双腿 OCO，触发后市价成交。
- **机器人宕机 SL/TP 依然有效**，由 OKX 服务器触发。
- 单独修改一腿时（如「把 BTC 止损上移」），另一腿价格自动保留，不会丢。

### 平仓

- 全平走 OKX close-position 接口（自动撤掉该仓位的策略委托）；部分平仓走 reduce-only 市价单。
- 平仓后从 positions-history 拉取交易所计算的真实盈亏用于推送。

### 服务端平仓检测（CheckStops）

周期轮询 positions-history，发现以下平仓会推送 Telegram：

- 止损/止盈触发（通过 algo 历史单匹配类型）
- 爆仓 / ADL（强平记录）
- 在 OKX 网页手动平仓

机器人自己发起的平仓不会重复推送（内部记录了已发起平仓的去重表）。

### 交易节流（与 paper 一致）

最小持仓 90 分钟、噪声区间不平仓、平仓后 4 小时同币种禁止再入场——live 模式同样生效，
防止 AI 频繁开平被手续费磨损。紧急情况（亏损 ≤ -3% 或盈利 ≥ +8%）可提前平，
Telegram 自然语言指令（「平掉 BTC」）不受节流限制。

## 5. 安全机制

- **缺 key 快速失败**：`TRADING_MODE=live` 而未配 OKX key 时启动直接报错。
- **demo/实盘醒目标识**：demo 启动打 `🎮`，实盘打 `🚨 ... 所有开平仓均为真实交易！`。
- **签名不随重定向外发**：所有私有请求禁止 HTTP 重定向携带凭据（`CheckRedirect`
  返回 `ErrUseLastResponse`），响应体限制 8 MB。
- **AI 风控约束继续生效**：Telegram 让 AI 平仓/改止损仍走风控方向校验
  （long 的止损必须低于现价等），非法参数直接拒绝。

## 6. 已知限制

- 仅 REST 轮询（5 分钟决策周期 + 90s 止损检查），无 WebSocket 行情；SL/TP 交给
  交易所侧实时触发弥补延迟。
- `TRADING_MODE=live` 时 SQLite 仍记录决策与权益快照（供 `/status` 与胜率统计），
  但真实持仓/权益以 OKX 账户为准；paper 账本的持仓表在 live 模式下不再写入。
- API key 未绑定 IP 白名单时 OKX 会报 `50113 该服务需完成验证`。
- 不支持组合保证金账户（single-ccy / spot-diy 模式未验证）。

## 7. 上实盘检查清单

1. `OKX_DEMO=1` 跑通一个交易日：开仓 → SL/TP 挂单可见 → 平仓推送 → `/positions` 正常。
2. 在 OKX 网页手动平一个仓，确认机器人能检测到并推送。
3. 重启机器人，确认持仓与 SL/TP 显示不丢（实时从 OKX 读取）。
4. 确认风控参数：候选币种、最大持仓数、单仓占比、最低信心度（Telegram「改策略」可调）。
5. 切 `OKX_DEMO=0`，从最小仓位开始观察。
