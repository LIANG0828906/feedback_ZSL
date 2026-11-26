# 实际验证结果

验证时间：`2026-11-27T02:37:51+08:00`

环境：

```text
go version go1.22.12 linux/amd64
```

复现命令：

```bash
go test ./...
go test -race ./...
./scripts/verify.sh
```

完整原始日志：`artifacts/verification-run.log`。

## 测试结论

`go vet ./...` 通过；`go test -v ./...` 中 17 个测试全部 PASS；`go test -race ./...` 通过，未报告数据竞争。

覆盖的关键验收：

- `TestAcceptanceThreePathsAndBounds`：全量、实时预算池、强杀后续算三路径一致，误差上界无违反。
- `TestSchemaValidationObjectAndField`：包围盒、格点越界/重合、SH 范围、指令时间均返回带对象和字段的 `E_SCHEMA`。
- `TestFixedReconstructionOrderIndependent`：反向项顺序不改变定点结果，截断记录完整。
- `TestQuantizationResidualMonotonic`：`1e2 → 1e6` 刻度收紧时量化残差单调不增。
- `TestParallelismAndShuffledChunksAreDeterministic`：1/2/8 worker 与随机分块顺序结果一致，小预算下记录排队。
- `TestTightenedToleranceDoesNotChangeMeasurement`：容差只改变认证/不确定标记，不改变实测偏差。
- `TestDependencyInvalidationIsExact`：辐亮度、分辨率、查询修改只失效实际下游，未受影响缓存可复用。
- `TestStateMachineIllegalTransitionPreservesState`。
- `TestCheckpointCorruptionHashAndVersionBoundary` 与 `TestTamperedCheckpointForcesWholeRoomRebuild`。
- `TestRoomExceptionIsolationDecodeAndRecovery`：样本损坏为 `E_DECODE`，单房失败可重试且不污染其他房间。
- `TestParameterInjectionGridOutOfBoundsIsEVal`。
- `TestDiffLocatesRoomCoordinateAndField`。

## CLI 验收输出

```text
seed=104373118593310 rooms=3 probes=135 directives=12 queries=137 pixels=128
versions=lp-recon-v1.0.0 param=params-standard-v1 fixed_scale=1000000
commit_order=[room-a room-b room-c]
realtime_pool workers=2 peak=2 waits=47 wait_total=7.051ms
resume_restored=[room-a] stale=0 order=[room-a room-b room-c]
error_bounds actual_max=2.073424e-06 accumulated_upper_max=7.375706e-04 violations=0
[PASS] schema, reconstruction, interpolation, bounds, graph and recovery acceptance
```

排队次数和等待毫秒会随调度在数十次（本机连续运行观测到 38–47 次）范围内变化；定点辐亮度、像素和提交顺序稳定。

## 强制终止与恢复

直接运行编译产物：

```text
checkpoint committed order=[room-a] path=.probebench/docs-final/manual/resume.lpjson
simulating hard termination with SIGKILL; no exit report is promised
crash_exit_code=137
[PASS] resumed order=[room-a room-b room-c] restored=[room-a]
[PASS] resume/full grid, pixels, uncertainty and commit order match; actual=2.073e-06 upper=7.376e-04
```

这验证了 SIGKILL 后只有已原子提交的 `room-a` 被恢复，`room-b`、`room-c` 续算，并且与一次性全量路径逐格点、逐像素一致。

## 工作台冒烟

```text
{"ok":true}
workbench_state True 135 36 249 2
```

`True` 表示内置验收的三路径 `AllConsistent`；三个数字分别是探针数、示例辐亮度编辑失效节点数、`room-c` 分辨率调整失效节点数和查询轨迹修改失效节点集合数（查询 + 像素）。

启动方式：

```bash
go run ./cmd/probebench serve --addr 127.0.0.1:8088 --dir .probebench/workbench
```

然后在 WSL/Windows 浏览器打开 `http://127.0.0.1:8088`。
