# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

环境：

- `go version go1.22.12 linux/amd64`
- `CGO_ENABLED=0`（本容器默认 CGO 工具链包装器失效；目标 Windows 也可免安装 gcc）
- 未使用 Docker、外部服务或网络账号。

## 1. 单元与集成测试

命令：

```bash
go test ./... -count=1
```

实际结果：全部包通过，根包包含生成器确定性、三路径、诊断 HTTP、版本不匹配恢复和 CLI 强杀恢复测试。完整输出保存在 `artifacts/verification-log/go-test.txt`。

```text
ok  github.com/liang/hapticfeedback
ok  github.com/liang/hapticfeedback/internal/haptic/branches
ok  github.com/liang/hapticfeedback/internal/haptic/checkpoint
ok  github.com/liang/hapticfeedback/internal/haptic/concurrency
ok  github.com/liang/hapticfeedback/internal/haptic/packetstate
```

CLI 强杀专项：

```bash
go test . -run TestCLIForceKillThenResumeAndReview -v -count=1
```

结果：

```text
=== RUN   TestCLIForceKillThenResumeAndReview
--- PASS: TestCLIForceKillThenResumeAndReview
```

该测试实际构建二进制，提交 2 个包后挂起，由测试进程 `Process.Kill()` 强杀；随后 `run --resume` 输出 `loaded_checkpoint: 2`、`packets: 5`，独立 `review` 返回 `"ok": true`。

## 2. 一键验收

命令：

```bash
CGO_ENABLED=0 go run . verify
```

实际 `artifacts/demo/report.json` 结论：

- `ok = true`
- 包数 = 5
- 定点采样点总数 = 39
- 三分支导入/指纹/逐帧比对：分支 1、2、3 全部 `ok=true`
- 路径：
  - `full_recompute`：5 包，一致
  - `realtime_sqlite`：5 包，一致
  - `checkpoint_partial`：持久化前缀 2 包
  - `resume_sqlite`：恢复 2 包并补齐到 5 包，一致
- 独立复核：`independent_review_ok=true`

## 3. 异常注入

验收报告中的实际定位：

| 注入 | 错误码 | 帧 | 通道 | 包序号 |
| --- | ---: | ---: | --- | ---: |
| 字段损坏 | `E_DECODE` | 3 | `ch_left` | - |
| 时序倒置 | `E_ORDER` | 3 | `ch_left` | - |
| 条件位矛盾 | `E_FLAG` | 6 | `ch_left` | - |
| 单包阶段失败 | `E_STAGE` | 6 | `ch_left` | 2 |

另有测试覆盖：未知分支/缺字段 `E_VERSION` 与路径、时间换算溢出 `E_OVER` 与两侧值、标志位缺失 `E_FLAG`、非法状态迁移 `E_STATE`、检查点哈希/版本篡改 `E_HASH`、复核器定位 `E_DIFF`。

## 4. 独立复核 CLI

命令：

```bash
CGO_ENABLED=0 go run . review --db artifacts/demo/resume.db --truth samples/truth.json
```

实际输出：

```json
{
  "ok": true,
  "packets": 5,
  "violation": null
}
```

## 5. 诊断界面冒烟

实际启动：

```bash
./artifacts/haptic serve --addr 127.0.0.1:18088 --samples samples --out artifacts/diag-smoke
```

HTTP 冒烟结果：

- `/` 包含“触觉反馈跨分支确定性合成内核”。
- `/api/state`：5 包、5 个状态机、60 条重映射痕迹、初始 `5/5` 提交、独立复核通过。
- `POST /api/inject?kind=stage`：返回 `E_STAGE packet_seq=2 channel=ch_left`。
- `POST /api/retry?seq=2`：包 2 重试成功并按序补齐，最终 `5/5`，复核通过。
- `/api/export?branch=2`：可导出分支 2 JSON。

## 6. Windows 交叉构建

命令：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o artifacts/haptic-windows.exe .
```

结果：成功生成约 14 MB 的纯 Go Windows amd64 可执行文件，无需 C 编译器或 SQLite 原生 DLL。
