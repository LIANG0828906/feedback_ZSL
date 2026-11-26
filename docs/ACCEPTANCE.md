# Acceptance Record

验证日期：2026-11-27  
环境：Linux，Go `1.22.12 linux/amd64`，无容器、无第三方 Go 依赖。

## 1. 单元与集成测试

```bash
$ go test ./... -count=1
?   	dialogueforge/cmd/samplegen	[no test files]
?   	dialogueforge/cmd/verify	[no test files]
ok  	dialogueforge/cmd/dialogue-workbench	0.009s
ok  	dialogueforge/dialogue	0.233s
```

内核测试共 17 项全部通过：

```text
TestGeneratedSampleEnvelopeMatchesTruth
TestExportLoadAllVersions
TestStateMachineRejectsIllegalTransitionWithoutMutation
TestIncrementalInvalidationAndCheckpointRestart
TestCheckpointHashMismatchInvalidatesWholeSegment
TestCheckpointVersionMismatchInvalidatesWholeSegment
TestUnreachableLoudnessReturnsELOUD
TestThreePathsAndDiffLocation
TestCheckpointConcurrentCommitsRemainParseable
TestFixedScaleRoundTripResidual
TestTimeConversionsAreIntegerAndTraced
TestScaleOverflowReturnsEOVFWithOperands
TestParallelMeasurementIsDeterministic
TestGeneratorIsDeterministic
TestSchemaErrorsCarryDialogueAndPath
TestV1UpgradeRecordsDefaults
TestDowngradeUpgradePreservesBoundariesAndGainWithinResidual
```

工作台另有 `TestWorkbenchStateAndEditEndpoints`，验证状态 API 和片段增益编辑 API。

## 2. 静态检查与竞态

```bash
$ go vet ./...
<无输出，退出码 0>

$ go test -race ./dialogue -count=1
ok  	dialogueforge/dialogue	3.443s
```

状态机、检查点并发提交、并行包络测量均未报告竞态。

## 3. 固定种子样本

```bash
$ go run ./cmd/samplegen -out samples -seed 42
$ sha256sum samples/...
5b4acef6cc4bc5d19524492668c4a53edfcf4e19bcfa9f6b0d61c910a821be0e  samples/dialogue-v1.json
9308a8d43841d35f0e3a425e1977ad232c67789ba245e940814b60e3ffc98eb0  samples/dialogue-v2.json
e8e5f8c4f89706f8e1803fc14e26cdfccaea24a314d0acdbb0b8d1f1138cda4a  samples/dialogue-v3.json
b4702a976d270c2546c4a3ca156d0e31db5db7ffb20876571bc23b81fe8b3e06  samples/manifest.json
```

连续生成两次后，上述四个文件的 SHA-256 完全一致：

```text
sample generation stable: yes
```

样本规模：

- 采样率 16,000 Hz；
- 18,560 个单声道 `pcm_s16le` 采样点；
- 2 个场景、4 个片段、21 个音素；
- 帧长 320、帧移 160，共 112 个逻辑帧；
- 内部目标响度 5600。

## 4. 三版本装载、换算与精度

```bash
$ go run ./cmd/verify -seed 42
```

关键结果：

| 版本 | 装载 | 语义差异 | 包络最大误差 | 声明容差 | 校准最大误差 | 全部达标 | E_LOSSY | 换算痕迹 |
| --- | --- | ---: | ---: | ---: | ---: | --- | ---: | ---: |
| v1 | 是 | 0 | 5 | 5 | 5 | 是 | 23 | 282 |
| v2 | 是 | 0 | 0 | 2 | 0 | 是 | 4 | 2 |
| v3 | 是 | 0 | 0 | 2 | 0 | 是 | 0 | 0 |

v1 的误差来自千分比响度真值的量化：内部万分比值取整到千分比后最大变化 5。v1 的 23 条 `E_LOSSY` 为 2 个不可表达的嵌套场景组和 21 条被折叠为常量基频的 F0 曲线。v2 的 4 条为 2 个场景显示名和 2 个无法落在整毫秒上的 F0 折点。v3 无降级项。

所有导出文件均被对应版本加载器成功读回；`CompareSemantic` 在目标版本可表达字段上不一致数为 0。

## 5. 三路径逐帧一致

`go run ./cmd/verify -seed 42` 对同一编辑序列运行实时、恢复与全量路径：

```text
edits:
  segment-gain seg-02 = 9900
  target-loudness = 5700
  segment-gain seg-04 = 9000
```

结果：

```text
equal: true
commit order (all paths): seg-01 -> seg-02 -> seg-03 -> seg-04
frames (all paths): 112
last invalidation: seg.seg-04.calibration
realtime reused nodes: 125, rebuilt: 1
ACCEPTANCE PASS
```

最终全量报告：

```json
{
  "seed": 42,
  "dialogue_id": "dlg-seed-42",
  "frames": 112,
  "segments": 4,
  "direct_envelope_max_error": 0,
  "direct_calibration_reached": true,
  "result": "PASS",
  "three_paths": {
    "equal": true,
    "realtime": {
      "commit_order": ["seg-01", "seg-02", "seg-03", "seg-04"],
      "frame_count": 112
    },
    "resume": {
      "commit_order": ["seg-01", "seg-02", "seg-03", "seg-04"],
      "frame_count": 112
    },
    "full": {
      "commit_order": ["seg-01", "seg-02", "seg-03", "seg-04"],
      "frame_count": 112
    },
    "final_target_loudness": 5700
  }
}
```

`TestThreePathsAndDiffLocation` 还人为修改一段一帧的校准后包络，确认比较器返回 `E_DIFF`，定位为 `seg-02`、帧 `2`、字段 `envelope`。

## 6. 工作台实测

启动：

```bash
go run ./cmd/dialogue-workbench \
  -addr 127.0.0.1:18080 \
  -sample samples/dialogue-v3.json \
  -checkpoint artifacts/workbench-smoke.checkpoint.json
```

HTTP 检查：

```text
GET /api/health
{"algorithm":"dialogue-envelope-v1","status":"ok"}

GET /api/state
progress {"committed":4,"total":4}
scenes 2
segments 4
seg-01 envelope frames 31

POST /api/edit
{"kind":"segment-gain","segment_id":"seg-02","value":9900}
invalidation reason "segment-gain", invalidated nodes 1
seg-02 gain adjustment updated, all four segments committed
```

页面包含逐帧 canvas 曲线、逻辑帧播放头、场景/片段切换、片段状态、失效清单与预算占用条。临时 smoke 检查点已清理，不进入交付目录。

## 7. 已知口径

- v1/v2 的 `E_LOSSY` 是降级警告清单，不阻断导出；处置方式在每项 `Treatment` 中明确记录。
- v1 真值容差 5 个内部单位、v2/v3 容差 2 个内部单位，为 schema 定点精度的显式声明。
- 编辑后的工作台工程保留原始真值作为生成器参考；增量结果由 `BuildResult.Calibration` 提供，不回溯改写真值。
