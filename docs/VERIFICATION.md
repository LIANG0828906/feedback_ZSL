# 实际验证记录

记录日期：2026-11-24  
执行环境：WSL Ubuntu，Go 1.22.12，linux/amd64  
数据种子：`42`

Windows 官方安装步骤无法在当前 Linux/WSL 容器中实机执行；已提供等价的 PowerShell 命令和 `scripts/verify-all.ps1`。Go 实现不依赖平台特定 API，仅使用标准库文件读写。

## 1. 环境

```text
$ go version
go version go1.22.12 linux/amd64

$ go env GOVERSION GOOS GOARCH
go1.22.12
linux
amd64

$ go list -m all
gaitretarget
```

无外部模块依赖。

## 2. 一键验收

```bash
./scripts/verify-all.sh
```

完整原始输出：`artifacts/verification-log.txt`。核心输出：

```text
generated seed=42 sources=2 targets=6 dir=./data
  source=src-walk-001 frames=168 cycles=3
  source=src-walk-002 frames=180 cycles=3
validation ok: sources=2 phases=2 targets=6 truths=2 params=params-seed-42-v1 algorithm=phase-locked-retarget-v1
full retarget complete: frames=138 events=11 corrections=0
three-path verification OK: source=src-walk-001 target=target-compact-01 frames=138 cycles=3 realtime_loaded=0 resume_loaded=2
three-path verification OK: source=src-walk-001 target=target-compact-01 frames=138 cycles=3 realtime_loaded=0 resume_loaded=1
three-path verification OK: source=src-walk-001 target=target-human-02 frames=168 cycles=3 realtime_loaded=0 resume_loaded=1
three-path verification OK: source=src-walk-001 target=target-stylized-03 frames=198 cycles=3 realtime_loaded=0 resume_loaded=1
three-path verification OK: source=src-walk-002 target=target-compact-01-s2 frames=148 cycles=3 realtime_loaded=0 resume_loaded=1
three-path verification OK: source=src-walk-002 target=target-human-02-s2 frames=180 cycles=3 realtime_loaded=0 resume_loaded=1
three-path verification OK: source=src-walk-002 target=target-stylized-03-s2 frames=212 cycles=3 realtime_loaded=0 resume_loaded=1
```

`realtime_loaded=0` 表示实时路径使用全新的检查点目录；`resume_loaded=2` 表示中断后第二个引擎确实从 2 个已提交周期恢复。

## 3. 三路径矩阵

| 源 | 目标 | 腿长比 | 趾关节 | 输出帧 | 三路径一致 |
| --- | --- | ---: | ---: | ---: | --- |
| `src-walk-001` | `target-compact-01` | 0.82 | 1 | 138 | 是 |
| `src-walk-001` | `target-human-02` | 1.00 | 2 | 168 | 是 |
| `src-walk-001` | `target-stylized-03` | 1.18 | 3 | 198 | 是 |
| `src-walk-002` | `target-compact-01-s2` | 0.82 | 1 | 148 | 是 |
| `src-walk-002` | `target-human-02-s2` | 1.00 | 2 | 180 | 是 |
| `src-walk-002` | `target-stylized-03-s2` | 1.18 | 3 | 212 | 是 |

`artifacts/verification-report.json`：

```json
{
  "equal": true,
  "source_id": "src-walk-001",
  "target_id": "target-compact-01",
  "frame_count": 138,
  "committed_cycles": 3,
  "realtime_loaded": 0,
  "resume_loaded": 2,
  "snapshot_invalidated": false,
  "interrupted_after": 2
}
```

## 4. 测试与静态检查

```text
$ go test ./... -count=1
ok gaitretarget/internal/gait

$ go vet ./...
（无输出）

$ go test -race ./... -count=1
ok gaitretarget/internal/gait
```

详细用例输出：`artifacts/test-results.txt`。

通过的测试：

| 测试 | 验证内容 |
| --- | --- |
| `TestDeterministicGeneration` | 同种子 JSON 逐字节一致 |
| `TestValidationErrorCodes` | `E_SCHEMA`、`E_CYCLE`、`E_PHASE` |
| `TestInjectionErrorCodes` | `E_JUMP`、`E_RATIO`、`E_DUR`、`E_PARA` |
| `TestMappingTraceAndAlignment` | 周期/帧映射、占比、`E_ALIGN` |
| `TestFullRetargetInvariantsAndHeightError` | 地面约束、滑动上界、趾关节、`E_HEIGHT` |
| `TestCycleStateMachine` | 合法/非法迁移、原状态保持 |
| `TestPauseBeforeCycleBoundary` | 暂停和恢复 |
| `TestCheckpointResumeInvalidationAndFreeze` | 恢复续跑、内容篡改全失效、版本不匹配全失效 |
| `TestThreePathsAndDivergence` | 三路径一致、`E_DIVERGE` 首差异定位 |
| `TestFailureIsolationRetryAndCommittedFreeze` | 单周期失败隔离、参数调整只影响未提交周期、重试成功 |
| `TestCorrectionRecordAndConcurrency` | 一次局部校正及三路径一致、串行/并行一致、检查点顺序、`E_BUFFER` |

## 5. 关键验收结果

- 相位占比：6 个目标均通过 `phase_ratio_tolerance=0.06` 检查。
- 支撑脚：所有输出支撑帧的 Y 等于地面，输出水平速度为 0，小于 `stance_slide_max=0.002`。
- 足部事件：`src-walk-001 -> target-compact-01` 全量输出 11 个事件，与真值事件数量一致，且逐事件落在 `landing_tolerance_frames=2` 内。
- 正常样本局部校正数为 0；测试注入不一致相位后产生 `stance -> double_support` 校正记录，三条路径包含相同校正记录。
- 检查点篡改一个根平移字段后，重启加载报告 `Invalidated=true`，旧周期文件全部删除，重算结果仍与全量基准一致。
- 参数版本变化后，重启加载同样整体失效，已提交周期数归零。
- `max_parallelism=1` 与 `max_parallelism=4` 输出逐样本一致。
- 检查点文件名严格为 `cycle-000000.json`、`cycle-000001.json`、`cycle-000002.json`，顺序稳定。

## 6. 复现校验和

```text
cda11266cc9f00c489f55f764ac061c7efa17b3b58328f8cc8969ebdc9ca7d29  data/params.json
fb8559b483f425e887b76c1b2810cb1e50211e7bff3a01ddfd41677ac8dd7bc9  data/phases.json
3f0a000d67819fa0d9f000a3b19a285bdb134c7337b98a01fd622b969479a0d9  data/sources.json
3f91b22f80c18104b9869cceeeab7815af83f7cde77947ed5516fb6f5649ba15  data/targets.json
8700a4dfd49b62d3c85aa51da8d433ad819b0e2f95727c9ce169c88fe5da764b  data/truths.json
3cf163c947c627700bddaead318b138f2db9711bb17ba70f6b7611628b8ab01d  artifacts/full-result.json
608e57faf0f4d70c861c340e2a94744ec05d9f53be36bb63eeb2612e36e92a71  artifacts/mapping.json
f64953640d4032ca13a5579d411675947bb6e1b28c5aabbaa84a74108380ff0e  artifacts/verification-report.json
4d85df27de56ec63da68a39918ea8bfa015c3fa54b2cf762bfaec6bd09740ffc  artifacts/verification-log.txt
59b8f7cd3b074c43ff73a02432fdeb95f515f2147fc5cd24af529ec03bdfe165  artifacts/test-results.txt
```

重新执行 `./scripts/verify-all.sh` 会重写数据与报告；Go 版本或算法版本变化时校验和可合理变化。
