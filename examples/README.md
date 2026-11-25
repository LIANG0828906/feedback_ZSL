# 示例

`sample_seed42.json` 由以下命令确定性生成：

```bash
go run ./cmd/acoustic-demo -serve=false -sample-out examples/sample_seed42.json
```

可直接通过 `acoustic.LoadProjectFile` 加载。清单中的 `truth` 由 `internal/samplegen` 的解析构造独立产生。
