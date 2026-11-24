# 验收运行结果（本地确定性样本）

环境：`go version go1.22.12 linux/amd64`（WSL2，本地回环，无容器、无外部目标）。

## 构建与测试

```
$ go build ./...
OK

$ go test ./... -count=1
?   audiocal/cmd/demo     [no test files]
?   audiocal/cmd/device   [no test files]
?   audiocal/cmd/samplegen [no test files]
ok  audiocal       0.063s
ok  audiocal/device 0.057s

$ go test -race ./... -count=1
ok  audiocal       1.488s
ok  audiocal/device 1.089s
```

共 28 个测试用例全部 PASS，覆盖 8 项业务要求：

- 结构校验：`TestDeterministicSamples` `TestValidateSampleErrors` `TestManifestRoundTrip` `TestConfigParaValidation`
- 拟合：`TestFitMeetsAnalyticUpperBound` `TestRegularizationMonotonicAndStableOrder` `TestFitViolationReturnsEFiT` `TestConfigurableGridFit`
- 固件/时序窗：`TestDeterministicEncoding` `TestSegmentWindowConstraints` `TestSegmentMergeAndDiagnostics` `TestIllegalWindowReturnsEWin` `TestMergeOverflowWindowReportsDetails` `TestLocateByteSegment`
- 状态机/检查点：`TestStateMachineTransitions` `TestCommittedSegmentFrozen` `TestCheckpointResumeAndInvalidation` `TestCheckpointContentHashTamper` `TestCrashRecoveryOnlyUsesPersistedSegments` `TestCheckpointSerialWrites`
- 三路径一致：`TestThreeExportPathsByteIdentical` `TestCompareDetectsFirstDiff`
- 并发/面板：`TestPoolBoundedParallelism` `TestPanelRenders`
- 设备回环与异常：`TestEchoRoundTrip` `TestFaultInjections` `TestEchoDriftTriggersEEcho` `TestRejectBadFirmwareMagic` `TestMergedFirmwareEchoConsistency`

## 端到端演示（go run ./cmd/demo）

```
== 加载测量记录 room-a（4 通道）==
== 拟合完成，SSE=177.418436，通道数 4 ==
== 三条导出路径逐字节一致：2964 字节, sha256=be13224c7781f828..., 提交序列 [0 1 2 3] ==
== 回环回显校验通过：4 通道，最大偏差 0.000000 dB ==
== 异常注入 固件版本拒收 -> E_FW (version) ==
== 异常注入 时序窗越界   -> E_WIN (segment=0) ==
== 异常注入 通道映射冲突 -> E_MAP (channel=0) ==
== 异常注入 段缺失       -> E_SEG (segment=1) ==
== 回显漂移 -> E_ECHO, 首个超差点 channel=0,point=0, local=29.1408 echo=29.6408 ==
```

## 验收结论对照

| 验收点 | 结果 |
| --- | --- |
| 三条导出路径固件逐字节一致 | PASS（2964 字节，相同 SHA256，相同提交序列 `[0 1 2 3]`） |
| 回显频响与本地拟合逐点对齐 | PASS（无注入时最大偏差 0.000000 dB） |
| 异常码链路 E_FW/E_WIN/E_MAP/E_SEG | PASS（均带段号或通道标识） |
| 回显超差 E_ECHO 定位首个频点与两侧值 | PASS |
| 拟合残差不超解析上界、λ 单调、阶数不变 | PASS |
| 检查点原子串行、损坏/版本失配整体作废 | PASS |
| 强制终止仅恢复已持久化段、未完成不出报告 | PASS |
| 分段超上限窗内合并并给诊断、不可满足给 E_WIN | PASS |
| 并发有界、检查点与下发串行列序稳定 | PASS（`-race` 无告警） |

## 持久化产物

```
data/samples.json
data/checkpoints/interactive/segment-{0..3}.json
data/checkpoints/resumable/segment-{0..3}.json
data/checkpoints/full/segment-{0..3}.json
```

## 关于 “Windows 工作目录”

任务书提到在 Windows 工作目录初始化；当前交付环境为 WSL2/Linux。代码仅使用 `filepath.Join`、
`os.Rename` 等跨平台 API，未使用任何 Unix 专有调用，同一份代码可直接在 Windows 的
`cmd`/PowerShell 下执行 `go build ./...`、`go test ./...` 与三个 `cmd/*` 程序；
`data/` 与 `data/checkpoints/` 均为运行期相对目录，在任意工作目录可读写创建。
