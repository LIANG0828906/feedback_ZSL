# 实际验证记录

验证日期：2026-11-25（Asia/Shanghai）

## 环境

- Go：`go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.34.5`（纯 Go）
- CGO：启用与否均不依赖 SQLite C 库
- Windows 构建：`GOOS=windows GOARCH=amd64 go build -o bin/uvtool-windows-amd64.exe ./cmd/uvtool` 成功，产物约 9.5 MB
- 未使用 Docker、GPU、外部账号或网络服务

## 自动测试

命令：

```bash
go test ./...
go vet ./...
```

结果：全部通过。覆盖：

- 固定种子样本生成确定性；
- 真值清单与三平面内容一致；
- 缺失元数据 `E_META`、缺字段 `E_META`、平面尺寸/位深不符 `E_PLANE`；
- 参数越界 `E_PARA` 与字段名；
- 滤波输出分辨率和位深保持；
- 单参数变化只失效对应阶段及下游；
- 单平面输入变化只失效该平面链及交织；
- 版本、输入哈希和输出载荷篡改导致整个快照作废；
- 取消后只保留部分快照，重启只补算缺失阶段；
- 1/4 工作线程输出逐像素一致；
- 缓冲耗尽阻塞并记录原因；
- 热力图重复生成逐字节一致；
- `E_DIFF` 定位首个差异像素及阶段；
- 误差四项和累积上界关系。

## 三路径比对

命令：

```bash
./scripts/verify_three_paths.sh
```

固定编辑序列：

1. 初始参数；
2. 只修改 U 增益/偏移；
3. 修改亮度锐化与局部对比度；
4. 修改 V 带宽和增益。

取消点：第 2 步提交 1 个新阶段后优雅取消，再重启。

### 320x240 样本

```text
sample_id: game_patch_320x240
incremental recompute/cache_hit: 16/16
recovery    recompute/cache_hit: 16/23
full        recompute/cache_hit: 8/0
final digest: sha256:88b3fe9eaed8dfad2ed34db4582608b782afcece44a95be65addfa3f4f2357c1
heatmap digest: sha256:a78aec258971fb532a88daead5f25f76e530531a13055144e12499c8c6b1c11e
comparison: status=identical, 8 stages, intermediate_planes_identical=true
commit order: incremental == recovery (16 entries, canonical topological order)
final snapshot stage order: identical across incremental/recovery/full
cancel: complete_after_cancel=false, persisted=[chroma_u.gain], resume_recomputed=[interleave]
```

### 1608x904 样本

```text
sample_id: game_patch_1608x904
incremental recompute/cache_hit: 16/16
recovery    recompute/cache_hit: 16/23
full        recompute/cache_hit: 8/0
final digest: sha256:320ec47af575ae10c76062a172caa541d1cd8b29c58e7f9d2784b06a51c5d723
heatmap digest: sha256:c14b40ec370a57b611350b23fdbbb2382209650fbaf26b7231d84b0b169eab9c
comparison: status=identical, 8 stages, intermediate_planes_identical=true
commit order: incremental == recovery (16 entries, canonical topological order)
final snapshot stage order: identical across incremental/recovery/full
cancel: complete_after_cancel=false, persisted=[chroma_u.gain], resume_recomputed=[interleave]
```

增量统计说明：四步序列各自重算 8、2、3、3 个阶段并命中 0、6、5、5 个阶段，总计重算 16、命中 16。恢复路径多出的 7 次命中来自重启时直用已提交的 7 个阶段；重启只补算 `interleave`。

## 取消/重启与全量输出比对

命令：

```bash
./scripts/cancel_restart.sh
```

结果：

```text
expected graceful cancel exit 7: observed
partial_run.json complete=false: verified
restart enhanced.yuv444p == full enhanced.yuv444p: byte-identical
restart difference.pgm == full difference.pgm: byte-identical
cancel persisted a partial snapshot; restart matched full recomputation byte-for-byte
```

## 误差报告

命令运行后生成 `work/verify-*/error_report.json`。示例（320x240，最终参数）：

- `used_truth_deviation=false`
- `bound_uses_observed_differences=false`
- 四项均非负，`cumulative_bound_quant_units` 等于四项之和
- 上界字段由采样比 `0.5`、插值阶数 `1`、量化步长 `1` 和声明参数计算

报告中的邻域二阶差分/一阶差分仅用于诊断，不参与上界计算。

## Windows

本容器验证了 Windows amd64 交叉编译。Windows 上可直接运行：

```powershell
.\scripts\generate_samples.ps1
.\scripts\verify_three_paths.ps1
.\scripts\cancel_restart.ps1
```

PowerShell 脚本以 `$LASTEXITCODE` 校验取消码 7，并用 SHA-256 比较重启与全量的增强图像和热力图。
