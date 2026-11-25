# 验证记录

验证日期：2026-11-25  
环境：Linux amd64，Go 1.22.12，`modernc.org/sqlite v1.34.5`

## 命令

```bash
go mod tidy
go vet ./...
go test ./...
go test -race ./...
go run ./cmd/samplegen -out samples -seed 20261125 -batches 3
```

结果：

- `go vet ./...`：通过。
- `go test ./...`：通过。
- `go test -race ./...`：通过。
- 样本生成：3 批 × 3 schema，共 58 个文件，约 2.2 MiB。

## 三版本逐像素验收

对 3 个 batch、3 个 schema 分别执行 `vectorrender`，共 117 个采样时刻：

```text
rendered 13 samples, match=true, maxPixelDelta=0
```

9 次运行全部 `maxPixelDelta=0`。示例同时核对了：

- 解析面积。
- 路径与整帧包围盒。
- 每段贝塞尔五个解析采样点。
- 参考像素 SHA-256。
- 位图宽高。

## 断点恢复

先对 `samples/batch-01/scene.v3.json` 建库并渲染，再使用同一 SQLite 文件不加 `-fresh` 重新运行：

```bash
out/bin/vectorrender ... -db out/checkpoints/b01-v3.db -fresh
out/bin/vectorrender ... -db out/checkpoints/b01-v3.db
```

两份 `diff-report.json` 使用 `cmp` 比较返回 0，说明提交时刻、提交序号、尺寸、包围盒和像素 SHA 一致。

自动化测试还覆盖：

- 实时预览、SQLite 持久化、断点恢复、空缓存全量重算四条路径的 timeline 逐项一致。
- 注入 coverage 故障后进度停留在已提交前缀，失败阶段为 `failed`。
- 清除故障并 `RetryTime` 后结果与全量重算一致。
- 破坏 SQLite payload 后恢复返回 `E_DECODE`。

## Windows 交叉构建

```bash
GOOS=windows GOARCH=amd64 go build -o out/windows/samplegen.exe ./cmd/samplegen
GOOS=windows GOARCH=amd64 go build -o out/windows/vectorrender.exe ./cmd/vectorrender
GOOS=windows GOARCH=amd64 go test -c -o out/windows/vectorcore.test.exe .
```

三个 Windows amd64 产物均构建成功。SQLite 使用纯 Go 驱动，不需要 CGO 或容器。

## 单元与集成覆盖

| 验收点 | 测试 |
|---|---|
| 三版本加载、源版本、错误上下文 | `TestLoadThreeVersions`、`TestLoadErrors` |
| v3→v2→v3 与时间/坐标往返 | `TestHighLowHighRoundTrip` |
| 容差单调、闭合、参数错误 | `TestFlattenMonotonic` |
| 覆盖、合成、`E_MODE` | `TestRasterCompositeAndModeErrors`、`TestInvalidOpacityReportsPath` |
| 状态机、冻结、进度 | `TestStateMachineAndFreeze` |
| 精确失效与增量/全量 | `TestPreciseInvalidationAndIncrementalFull` |
| 故障重试、依赖图 | `TestRetryInProcessAndDependencies` |
| 预览/恢复/全量一致 | `TestPreviewResumeFullConsistency` |
| 检查点损坏、硬上限 | `TestCheckpointCorruptionAndLimits` |
| `E_DIFF` 首个差异定位 | `TestDiffLocatesFirstMismatch` |
| 生成样本三版本与真值 | `TestGeneratedSamplesAllVersions` |

