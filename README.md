# 游戏对白声学特征与响度校准内核

纯 Go 1.22+、仅标准库的可集成领域组件与本地诊断台。固定种子 `42042`
生成单声道 S16 PCM、三版本 schema 工程和解析真值；不需要音频设备、Docker、
账号或外网服务。

原始任务书保留在 [`TASK_SPEC.md`](TASK_SPEC.md)，实现映射与验证记录见
[`TECH_NOTES.md`](TECH_NOTES.md)。

## 快速运行

```bash
go version
go test -count=1 ./...
go vet ./...

go run ./cmd/dialogue generate -out samples
go run ./cmd/dialogue verify   -out artifacts
go run ./cmd/dialogue serve    -addr 127.0.0.1:18086
```

也可以使用 Make 目标：

```bash
make test
make generate
make verify
make serve
```

启动后打开 <http://127.0.0.1:18086/>。若该端口被占用，换一个回环端口即可。

## 交付内容

- `generator.go` / `reference.go`：确定性 PCM 与独立解析真值。
- `schema.go` / `export.go` / `roundtrip.go`：v1/v2/v3 装载、字段痕迹、降级
  `E_LOSSY` 清单和往返残差。
- `convert.go`：采样/毫秒、千分比/万分比、线性幅度/分贝的整数定点换算。
- `features.go`：帧量化、定点能量、过零率、瞬态峰和截断痕迹。
- `calibrate.go`：片段能量、逐帧响度、增益求解、预算与 `E_LOUD`。
- `state.go`：五态状态机和提交进度。
- `graph.go` / `cache.go` / `pipeline.go`：依赖图、缓存键、精确失效和增量重建。
- `checkpoint.go`：串行原子检查点、断点恢复和快照作废。
- `scheduler.go`：并行度、采样缓冲、队列预算和异常隔离。
- `server.go` / `web/index.html`：`net/http` API 与零外部依赖诊断页。
- `*_test.go`：29 个测试，覆盖验收路径和全部业务码。

## 机器可读结果

```bash
go run ./cmd/dialogue verify -out artifacts
```

写入：

- `artifacts/verification-report.json`
- `artifacts/diagnostic.json`

样本生成写入 `samples/dialogue-v1.json`、`samples/dialogue-v2.json`、
`samples/dialogue-v3.json`、`samples/truth.json` 和各版本损失报告。
