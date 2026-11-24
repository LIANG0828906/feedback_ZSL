# 运行手册与验证结果

## 环境

- Go：1.22.12（`go.mod` 声明 `go 1.22`，依赖已钉到兼容版本）
- 存储：`modernc.org/sqlite v1.29.10` 纯 Go 驱动，无 CGO、无外部服务
- 所有输入输出均为项目根下相对路径：`samples/`、`db/`、`logs/`

## 常用命令

```bash
go mod tidy
go build ./...
go test ./...
go test -race ./...
go run ./cmd/demo
go run ./cmd/verify -db db/m_medium_live.db -truth samples/truth/m_medium.truth.json
```

Windows PowerShell 下命令相同；交叉编译验证：

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

## 固定种子与规模参数

演示管线在 `cmd/demo/main.go` 中固定：

- 主种子：`20261124`
- 对局规模：60/800/20000 回合，单位数 4/6/8，两队均分
- 窗口大小：50 回合；并行度 4；每线程缓冲 8
- 异常注入回合：21（窗口 0），序列 decode/order/conflict/stage

## 本次实际验证结果

### 单元/竞态测试

`go test -race ./...` 全部通过，覆盖：

- 定点溢出与安全边界、PRNG 确定性
- `E_SCHEMA/E_ORDER/E_DECODE` 加载校验与字段路径
- 真值逐回合一致、三路径持久化一致
- 窗口状态机非法迁移保态、窗口重叠 `E_STATE`
- 两种自适应算法结果逐字段一致
- 检查点哈希/版本不匹配整体作废
- `E_CONFLICT/E_STAGE` 注入、失败窗口重试
- 并发池背压排队与结果顺序确定
- 独立复核篡改真值后定位首个违规回合与字段

### 演示管线（`go run ./cmd/demo`）

- 三规模对局 live/recovery/full 三路径窗口全部提交，两两比对 `IDENTICAL`。
- 20000 回合对局 = 400 窗口，三路径各 20000 回合快照，独立复核全部 `OK`。
- 异常链路：`E_DECODE`、`E_ORDER`（期望 21 实测 23）、`E_CONFLICT`、`E_STAGE` 均按窗口/回合定位。
- stage 失败窗口重试成功，提交 50 回合快照，其他窗口不受影响。
- 自适应决策默认预算下三场均选 `fine_grained`；压缩内存预算测试强制 `window_pre_aggregate` 且与精细路径一致。

### 独立复核 CLI

```text
$ go run ./cmd/verify -db db/m_medium_live.db -truth samples/truth/m_medium.truth.json
PASS match=m_medium checked_turns=800
```

复核工具以 `mode=ro&query_only=1` 只读打开 SQLite，不 import 结算引擎包。
