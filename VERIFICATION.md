# 本地验证记录

验证日期：2026-11-25  
运行环境：Go 1.22.12，`linux/amd64`，无 Docker，无外部服务  
固定种子：`20261125`  
默认参数：Sobel、3x3 Gaussian、flood=140、min-region=12、merge-cost=64、merge-area=5000、cleanup=1

## 1. 自动化测试

命令：

```bash
go test ./...
go vet ./...
go test -race ./internal/workpool ./internal/runner ./internal/diagnostics ./internal/checkpoint
```

结果：全部包通过，竞态检测通过。覆盖生成器确定性、清单/通道/尺寸/损坏校验、梯度与淹没逐像素确定性、开闭清理与统计、预览/精确一致性、版本混用、`E_DIVERGE`、非法状态迁移、哈希失效、暂停恢复、独立验证器、回环 HTTP、线程池边界与结果顺序。

## 2. 一次性全量运行

```bash
./artifacts/iconseg run --out artifacts/run --seed 20261125
./artifacts/iconseg verify --db artifacts/run/checkpoints.db
```

| 图标 | 预览倍率 | 预览耗时 | 精确区域数 | 参考掩码失配率 | 最大重心偏差 | 最大孔洞偏差 | 层提交数 |
|---|---:|---:|---:|---:|---:|---:|---:|
| icon_ring_96 | 2x | 2.264 ms | 1 | 0.271% | 0.030 px | 0 | 36 |
| icon_pair_112x88 | 2x | 2.385 ms | 2 | 0.599% | 0.041 px | 0 | 48 |
| icon_triad_128x120 | 2x | 2.736 ms | 3 | 0.495% | 0.057 px | 0 | 64 |

预览预算：`60 ms`，三张图均在约 `2-3 ms` 内返回。

预览/精确一致性：

| 图标 | 区域数 | 最大重心偏差 | 最大面积相对误差 | 差异像素比例 | 判定 |
|---|---:|---:|---:|---:|---|
| icon_ring_96 | 1 = 1 | 0.030 px | 5.708% | 1.964% | 通过（2 px / 8% / 5%） |
| icon_pair_112x88 | 2 = 2 | 0.059 px | 4.397% | 1.674% | 通过 |
| icon_triad_128x120 | 3 = 3 | 0.070 px | 5.252% | 2.135% | 通过 |

独立验证器对三个快照均返回 `valid=true`、`stats_mask_mismatch_pixels=0`、`commit_order_stable=true`，重建掩码哈希和统计哈希与持久化值完全一致。

## 3. 暂停与断点恢复

第一次进程每张图提交 18 层后退出：

| 图标 | 已提交 | 总层数 | 快照状态 |
|---|---:|---:|---|
| icon_ring_96 | 18 | 36 | building |
| icon_pair_112x88 | 18 | 48 | building |
| icon_triad_128x120 | 18 | 64 | building |

同一 SQLite 文件重启后：

| 图标 | 跳过已提交 | 新提交 | 最终状态 |
|---|---:|---:|---|
| icon_ring_96 | 18 | 18 | complete |
| icon_pair_112x88 | 18 | 30 | complete |
| icon_triad_128x120 | 18 | 46 | complete |

一次性运行与恢复运行的三组结果比较：

```text
commit sequences identical
mask, stats, and consistency identical
```

最终掩码哈希：

```text
icon_ring_96      7af2dacf87ae12bb88a8648df1a53fd924dd4bcc8706d0c928c807e6068cffce
icon_pair_112x88  fd9a3093deeef96d1c2564d44ac5a8c93190396b4726421612c0690c55f8b985
icon_triad_128x120 76a8ece034b4f62e9615643fc6bc5591a93817ee17dc93899aa73ea7ae620848
```

恢复后的快照均通过独立验证器。

最终源码重新构建后又执行了一次独立的一次性全量运行，输出位于
`artifacts/final-run/`。其掩码哈希、统计哈希和完整提交序列与
`artifacts/run/` 逐项相同，三个快照再次通过独立验证器。

## 4. 本地 HTTP 诊断页面

```bash
./artifacts/iconseg serve --addr 127.0.0.1:18091 --db artifacts/serve/checkpoints.db
curl 'http://127.0.0.1:18091/api/preview?flood=140&merge_cost=64'
curl 'http://127.0.0.1:18091/api/exact?flood=140&merge_cost=64'
```

实际结果：

```json
{
  "preview": {
    "downsample": 2,
    "elapsed_ms": 1.41,
    "within_latency": true,
    "flood_samples": 3806
  },
  "exact": {
    "snapshot_status": "complete",
    "consistent": true,
    "committed_layers": 36,
    "total_layers": 36
  }
}
```

原始响应保存在 `artifacts/http-check.json`。非回环地址会被拒绝。

## 5. 构建产物

```bash
CGO_ENABLED=0 go build -trimpath -o artifacts/iconseg ./cmd/iconseg
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o artifacts/iconseg.exe ./cmd/iconseg
file artifacts/iconseg artifacts/iconseg.exe
```

```text
artifacts/iconseg:     ELF 64-bit LSB executable, x86-64, statically linked
artifacts/iconseg.exe: PE32+ executable for MS Windows 6.01 (console), x86-64
```
