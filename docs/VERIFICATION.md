# 实际验证记录

验证时间：2026-11-25（Asia/Shanghai）。

环境：

- Go `go1.22.12 linux/amd64`
- 纯 Go SQLite：`modernc.org/sqlite v1.29.10`
- 测试/构建命令统一设置 `CGO_ENABLED=0`
- 另执行 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build`，Windows amd64 构建通过

## 1. 全量测试

命令：

```bash
CGO_ENABLED=0 go test -count=1 ./...
```

实际结果：

```text
?       iconbench/cmd/verify    [no test files]
?       iconbench/cmd/workbench [no test files]
?       iconbench/internal/model        [no test files]
?       iconbench/internal/verify       [no test files]
?       iconbench/internal/web  [no test files]
ok      iconbench/internal/checkpoint   0.005s
ok      iconbench/internal/imaging      0.001s
ok      iconbench/internal/pipeline     0.317s
ok      iconbench/internal/sample        0.033s
ok      iconbench/internal/segment       0.128s
```

## 2. 样本生成与全量重算

命令：

```bash
CGO_ENABLED=0 go run ./cmd/workbench generate -data data
CGO_ENABLED=0 go run ./cmd/workbench recompute -data data
```

实际结果：

```text
generated 4 images in data
icon_grid_128 regions=4 iou=0.9965 mismatch=13 centroid=0.064 area=0.006 commits=16
icon_wide_192x128 regions=5 iou=0.9980 mismatch=10 centroid=0.039 area=0.005 commits=24
icon_tall_128x192 regions=6 iou=0.9965 mismatch=20 centroid=0.035 area=0.008 commits=24
icon_strip_256x128 regions=7 iou=0.9968 mismatch=20 centroid=0.023 area=0.005 commits=32
```

所有图像的预测区域数、孔洞数与真值一致；最大重心偏差约 `0.064px`，最大面积相对误差约 `0.8%`，显著优于声明容差 `2.5px` 和 `8%`。

## 3. 独立验证工具

命令：

```bash
CGO_ENABLED=0 go run ./cmd/verify -data data
```

实际结果：

```text
PASS icon_grid_128 mask=8721b39e7c71 commits=16 iou=0.9965 centroid=0.064 area=0.006
PASS icon_wide_192x128 mask=3a139170fa1b commits=24 iou=0.9980 centroid=0.039 area=0.005
PASS icon_tall_128x192 mask=d72907868fea commits=24 iou=0.9965 centroid=0.035 area=0.008
PASS icon_strip_256x128 mask=774aaffd92ea commits=32 iou=0.9968 centroid=0.023 area=0.005
```

验证器从 `flood_merge` 层持久化的块掩码重建全图，执行同一开闭清理，并逐块核对 `stats.clean_mask`、镜像结果哈希、提交顺序、内容哈希和版本。

## 4. API 实测摘要

在 `127.0.0.1:18080` 启动后，使用 `icon_grid_128`、参数版本 `7` 实测：

```text
POST /api/preview   -> HTTP 200, downsample=2, flood_samples=3728, elapsed=3ms, within_budget=true
POST /api/recompute -> HTTP 200, regions=4, exact IoU=0.99649, judgment.ok=true
```

同版本粗预览相对精确结果的实测最大重心偏差约 `0.497px`，最大面积相对误差约 `6.95%`，位于预览容差 `3.5px` / `14%` 内。

版本混用：

```json
{
  "code": "E_VER",
  "message": "parameter version mismatch: preview=v6 exact=v7",
  "version_a": "v6/83021484d4dafecbaa2485fe/algo-1.0.0",
  "version_b": "v7/83021484d4dafecbaa2485fe/algo-1.0.0"
}
```

参数越界：

```json
{
  "code": "E_PARA",
  "field": "flood_threshold",
  "min": 1,
  "max": 255
}
```

## 5. 三路径一致性的自动化证据

`internal/pipeline/consistency_test.go` 中断言：

- 全量重算、停止交互后的精确预览路径、断点恢复路径的掩码 `bytes.Equal`。
- 连通域统计 `reflect.DeepEqual`。
- 三条路径的 `(block_index, layer, content_hash)` 提交序列完全一致。
- 1 worker 与 4 worker 的 `mask_hash` 相同。
- 恢复后的 SQLite 快照通过独立验证器。
- 篡改任一检查点哈希后，下一次运行设置 `snapshot_reset=true`，整体删除旧快照并重算出相同掩码。

## 6. Windows 构建

命令：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/IconBench.exe ./cmd/workbench
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/verify.exe ./cmd/verify
```

结果：`WINDOWS_BUILD_OK`，生成 `bin/IconBench.exe` 与 `bin/verify.exe`。
