# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）

环境：

- WSL2：`Linux 6.18.33.1-microsoft-standard-WSL2 x86_64`
- Go：`go version go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.29.10`（纯 Go，无外部服务）
- 网络：仅在初次拉取 Go 模块依赖时使用配置的 Go module proxy；测试和演示均离线执行。

## 必跑命令

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
go run ./cmd/tunnelbench verify --workdir artifacts/verify
```

实际结果：

- `go build ./...`：通过，退出码 0。
- `go vet ./...`：通过，无诊断。
- `go test ./...`：
  - `ok tunnelworkbench/internal/forward`
  - `ok tunnelworkbench/internal/verify`
- `go test -race ./...`：通过，无 data race。
- `tunnelbench verify`：`43/43 checks passed`，无 FAIL。

机器可读完整报告：`artifacts/verify/verification-report.json`。
控制台存档：`artifacts/verification-console.txt`。

## 验收覆盖

43 项检查按类别分布如下：

- Schema 一致性：隧道 ID 次序、对端成员、规则优先序、裁决命中集合/路径、分片数量/字节签名。
- 状态机真值：三个 schema 各有 8 条逐事件迁移真值，覆盖 idle → handshaking → established → disconnected。
- 分片真值：8 个报文逐报文比较拒绝码、片数、每片载荷和总字节，容差为工程声明的 8 字节。
- 并行确定性：三个 schema 分别用 `workers=1,buffer=1`、`workers=2,buffer=2`、`workers=4,buffer=8` 比较顺序结果。
- 增量重建：
  - MTU 编辑仅失效 T2 分片依赖，序号 9 的大片重建、序号 8 的小片保持 hit；
  - 规则动作编辑传播 `rule -> verdict -> result:verdict -> fragment` 闭包；
  - 对端归属编辑重建相关状态和 T1 分片；
  - 加密套件编辑仅重建 cipher 依赖分片，状态项保持 hit；
  - 每次均与一次性全量时间线重算逐事件签名一致。
- 检查点：前 7 个事件提交后可恢复到 seq=7；状态先写、全局序号后提交的顺序通过“只写 state bundle、不提交 seq”的崩溃点测试；版本不匹配时 `full_replay=true` 并重放到 seq=18。
- 异常码：12 类注入全部返回预期 `E_xxx`。

## HTTP 工作台实测

已实际启动：

```bash
./artifacts/tunnelbench serve --addr 127.0.0.1:18088 --workdir artifacts/wb-test
```

并验证：

- `GET /api/overview` 返回 seed `42042`、18 个事件、8 个报文；
- `GET /api/equivalence` 返回 `"consistent": true`；
- `GET /api/simulate?schema=v3&parallel=1&workers=2&buffer=2` 返回 18 个事件；
- `POST /api/edit` 修改 MTU 后返回 `"consistent": true`；
- `POST /api/inject {"kind":"missing_peer"}` 返回 `E_PEER_NOT_FOUND`；
- `GET /` 能返回中文工作台 HTML。

## 样本生成

```bash
go run ./cmd/tunnelbench gen --out artifacts/sample
```

已生成：

- `artifacts/sample/engineering-v1.json`
- `artifacts/sample/engineering-v2.json`
- `artifacts/sample/engineering-v3.json`
- `artifacts/sample/events.json`
- `artifacts/sample/truth.json`
- `artifacts/sample/canonical-project.json`

## 已知边界

- 工作台是本地诊断工具，默认监听 `127.0.0.1`，没有设计多用户鉴权。
- 分片模型使用显式声明的统一内层 IPv4 头开销 20 字节和 8 字节对齐；这是本实现公开的确定性契约，不声称复刻任意厂商私有封装。
- 真实信号强杀（SIGKILL）无法被进程捕获，但两阶段提交序号保证重启只读最后完整事件；SIGINT/SIGTERM 会在事件边界优雅落盘。
