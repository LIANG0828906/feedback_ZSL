# Baseline

该目录由以下命令生成：

```bash
./build/scene_gen --out-dir data
./build/shadowpipe --scene data/synthetic.rscene \
  --config data/sample.script --output data/baseline
```

`SHA256SUMS` 覆盖阴影因子图、可见性掩码、误差热力图、摘要和快照。
`scripts/verify.sh` 会先校验基线文件完整性，再检查独立双渲染的逐字节一致性。

