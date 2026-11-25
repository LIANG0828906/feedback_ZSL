# 实际验证记录

验证时间：2026-11-25 13:31:00 +08:00  
环境：WSL2 / Linux x86_64，Go `go1.22.12 linux/amd64`，SQLite 驱动为纯 Go `modernc.org/sqlite v1.29.10`。  
说明：当前 WSL 未暴露 `powershell.exe` 或 `go.exe`，因此无法在此环境内原生执行 Windows 测试；已完成 Windows amd64 纯 Go 交叉编译，并提供 `scripts/validate.ps1` 供 Windows 原生运行。

## 1. 单元与端到端测试

命令：

```bash
go test ./... -count=1
```

结果：全部通过。

`go vet ./...` 同样通过。

```text
ok  qkernel/internal/attribution
ok  qkernel/internal/chain
ok  qkernel/internal/sched
ok  qkernel/internal/session
ok  qkernel/internal/synth
```

关键测试：

- `TestDefaultScenarioThreePathsAndVerify`：实时、一次性全量、7 次提交后中断恢复三路径的最终像素、指标、归因和提交序列一致。
- `TestPreciseInvalidationAndRollback`：强度修改后仅一个活动分段重算，远处下游步骤保持 `reused`；v1 → v2 → 回滚 v1 → 再前进 v2 与直接执行 v2 像素一致。
- `TestRegionChangeDoesNotInvalidateFarOperatorDomain`：作用区域移动后，远处算符域不失效。
- `TestOrderChangeReusesSpatiallyUnaffectedStep`：空间隔离步骤换序后，未受影响的第三步通过输入域哈希复用。
- `TestSegmentSizeAndParallelismDoNotChangePixels`：32px/4 worker 与 64px/1 worker 最终像素一致。
- `TestUnitFailureRetryAndStateIsolation`：单块注入失败为 `failed`，其他已提交单元不被污染，重试后独立校验通过。
- `TestStateMachineIllegalTransition`：`pending -> committed`、`committed -> processing` 等非法迁移返回 `E_STATE` 且不落库变更。
- `TestBufferBudgetQueuesWithoutDropping`、`TestUnitFailureIsolatesAndCanRetry`：缓冲竞争排队事件和异常隔离。
- `TestStepsPreserveOutsideRegionBytes`：五类算符对 ROI 外像素逐字节保持。
- `TestLoadTruthSchemaAndMetaValidation`：缺字段 `E_SCHEMA`，尺寸/通道冲突 `E_META`。
- `TestAmbiguousAttributionReturnsCandidates`：无步骤超过声明份额时返回 `E_ATTR` 和候选步骤。

竞态检测：

```bash
CGO_ENABLED=1 go test -race ./... -count=1
```

结果：全部通过，无 data race。

## 2. 本地样本端到端

命令：

```bash
go run ./cmd/qkernel demo --out demo-output --frames 3 --workers 4
```

实际结果：

```text
run: run_331f912388031d55e3f2b6f2d54f2b0f
final hash: 2221be9bd68699de85e94b49cd25e1aed4c2028fe2ce7eea51a746b76129db3c
commits: 135 (full=135 resume=135)
three-path verification: ok=true
```

独立验证：

```bash
go run ./cmd/qkernel verify \
  --root demo-output \
  --run run_331f912388031d55e3f2b6f2d54f2b0f
```

实际结果：

```text
verify ok: True
commits: 135
violations: 0
commit_sequence_hash: 00fbdc836367611d254e3677be22b6f116ecad2da4f754b9001b53c027693110
```

验证器不调用 `session.Engine.RunFrame` 主路径；它只打开 SQLite 提交记录、读取分段 blob、`base/*.rgb` 和真值 JSON，然后自行拼图、校验采样点、重算指标与归因。

## 3. Windows 构建

根 module：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

结果：

```text
GOOS=windows root go build: PASS
```

Wails 壳独立 module：

```bash
cd diagnostic/wails
GOTOOLCHAIN=local GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

结果：

```text
Wails shell GOOS=windows go build: PASS
```

在真实 Windows 桌面环境中建议执行：

```powershell
.\scripts\validate.ps1
cd diagnostic\wails
wails build
.\build\bin\qkernel-diagnostic.exe -root ..\..\demo-output
```

## 4. 诊断界面冒烟

```bash
go run ./cmd/qkernel serve --root demo-output --addr 127.0.0.1:8787
curl http://127.0.0.1:8787/
curl http://127.0.0.1:8787/api/state
```

首页包含“画质劣化基准内核诊断台”，API 返回算链、135 个单元状态、108 个归因块和 `verification.ok=true`。前端 HTML/CSS/JS 全部内嵌，不请求外部字体、CDN、图片或脚本。
