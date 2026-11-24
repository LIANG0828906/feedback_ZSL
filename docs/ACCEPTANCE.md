# 验收记录

## 环境

- 日期：2026-11-24 UTC
- Go：`go1.22.12 linux/amd64`
- GOPROXY：`https://proxy.golang.org,direct`
- GOFLAGS：空
- Wails CLI：`v2.10.2`
- SQLite driver：`modernc.org/sqlite v1.34.5`（纯 Go）
- 工作方式：本地确定性样本，无 Docker、GPU、账号或运行时网络

## 自动验收

命令：

```bash
go run ./cmd/hdrproof demo --root artifacts/demo --workers 4 --buffer 2
```

结果：`artifacts/demo/report.json` 中 `outcome = "pass"`。

| 项目 | 结果 |
|---|---|
| 路/帧/段 | 4 路 / 95 帧 / 18 段 |
| 真值指纹 | `26a1a5e4e3302d05e1491eb2e14951adcad0e4a89a8b95c5a89799f77756e263` |
| 实时 | 95 帧，95 records，4/4 路，18/18 段 |
| 断点中断 | 5/18 段，28 帧已持久化，`cancelled=true` |
| 断点完成 | 95 帧，95 records，4/4 路，18/18 段，`snapshot_reused=true` |
| 全量复算 | 95 帧，95 records，4/4 路，18/18 段 |
| realtime vs full | 相等，95 帧 |
| resume vs full | 相等，95 帧 |
| realtime vs resume | 相等，95 帧 |
| 独立审计 | 4 路、18 段、95 帧、95 records 通过 |
| 帧缓冲阻塞 | 110 次（35+20+22+33） |

实时与全量数据库文件的 SHA-256 均为：

```text
9522a36f301ae8dd92734177cdf817aeb83894efff35cd407e859150d6c4dd71
```

这提供了除逐帧比对之外的二进制层面旁证。恢复库因中断/恢复过程与提交批次不同，文件级哈希不要求相同，但逐帧像素与 proof 已由比对器确认一致。

## 注入链路

| 用例 | 期望 | 实际 | 结果 |
|---|---|---|---|
| bad_frame | `E_FRAME` | `E_FRAME` | 路、段、帧、字段齐全 |
| meta_conflict | `E_META` | `E_META` | `width expected=999: received 32` |
| parameter_out_of_range | `E_PARA` | `E_PARA` | `target_peak_nits expected=50..300` |
| segment_failure | `E_SEG` | `E_SEG` | 一次性失败后重试完成 18/18 段 |

## 测试与静态检查

```text
go test ./... -count=1                 PASS
go test -race ./... -count=1           PASS
go vet ./...                           PASS
cd desktop
go test ./internal/workbench -count=1  PASS
```

测试覆盖：

- 生成确定性、真值指纹、CRC 损坏、声明矛盾、缺失真值；
- 重复转换 byte 一致、参数错误上下文；
- 非法状态迁移不改状态、failed→running 重试；
- 三路径一致、已提交段冻结；
- 参数更新计划只作用于未提交段，计划变化整库失效；
- 版本/内容 hash 失配整体作废；
- 四类注入链路与隔离；
- 并发上限、缓冲阻塞、稳定 commit order；
- `E_DIFF` 首差异字段定位；
- `E_AUDIT` 缺记录与重复记录；
- Wails 工作台生成、运行、帧明细、审计、比对、导出、冻结段拒改。

## 桌面构建

当前 Linux 主机缺少 `libwebkit`，`wails doctor` 因此无法通过 GUI 运行时检查。但 Windows 目标构建成功：

```text
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build   PASS
wails build -platform windows/amd64 -skipbindings  PASS
产物：desktop/build/bin/hdrproof-workbench.exe
```

Windows 实机需按 `scripts/verify-windows.ps1` 执行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify-windows.ps1 -RunDev
```

日志写入 `artifacts/windows-*.log`。WebView2 运行时为 Wails Windows 桌面运行前提。

## 关键产物

- 样本：`artifacts/demo/samples/manifest.json`、`truth.json`、`routes/**/frames.hdrseq`
- 注入：`artifacts/demo/samples/injections/*.json`
- 快照：`artifacts/demo/realtime.db`、`resume.db`、`full.db`
- 注入快照：`artifacts/demo/injection-*.db`
- 报告：`artifacts/demo/report.json`
- 事件日志：`artifacts/demo/demo.log`
