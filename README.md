# MaskPipe：游戏美术掩码分割并行预览与验证工具

本地确定性样本 → 颜色/梯度区域生长 → 阈值与形态学清理 → 区域几何/真值匹配 → 受限预算并行分块 → SQLite 版本化检查点 → 预览/恢复/全量三路径一致性 → 独立验证与 Web 诊断。

## Windows 快速开始

需要 Go 1.22+。先确认：

```bat
scripts\check-env.bat
```

运行测试和完整示例：

```bat
scripts\test.bat
scripts\demo.bat
scripts\serve-demo.bat
```

然后打开 `http://127.0.0.1:8088`。

脚本显式设置 `CGO_ENABLED=0`；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，不需要 gcc、Visual C++ 或 Docker。

## 手动命令

```bat
set CGO_ENABLED=0

go test ./...
go run .\cmd\maskpipe generate -root samples\local-seed -seed 424242
go run .\cmd\maskpipe validate -sample samples\local-seed -scene blocks
go run .\cmd\maskpipe run -scene blocks -params configs\params.example.json -db reports\example-blocks.db -out reports\example-blocks.json -mode full -preview
go run .\cmd\maskpipe threeway -scene blocks -out reports\threeway-blocks
go run .\cmd\maskpipe verify -db reports\threeway-blocks\dbs\full.db -manifest samples\local-seed\manifest.json -scene blocks -out reports\threeway-blocks\verify-cli.json
go run .\cmd\maskpipe serve -scene blocks -db reports\web-diagnostic.db -addr 127.0.0.1:8088
```

可选场景：`blocks`、`low-contrast`、`embedded-noise`。

## 主要输出

- `samples/local-seed/manifest.json`：图像、真值几何、指纹、分块、预算和容差的唯一声明来源。
- `reports/<scene>.json`：单次运行、质量报告、预算排队、状态和阶段序列。
- `reports/<scene>.mask.png` / `.overlay.png`：最终掩码和叠加图。
- `reports/threeway-<scene>/three-way.json`：实时增量、断点恢复、全量重算比对。
- `reports/threeway-<scene>/independent-verification.json`：独立验证器报告。

## 已实现的异常链路

- 损坏 PNG → `E_DECODE`。
- manifest 缺字段、尺寸/通道/分块网格/指纹不符 → `E_META`，含字段名。
- 参数越界 → `E_PARA`，含 `color_tolerance`、`gradient_threshold` 或 `structure_size`。
- 区域数量不符 → `E_REGION`，列出 extra/missing 区域与面积。
- 质量超差 → `E_QUAL`，给出区域 ID、重心、面积和外接框偏差。
- 非法状态迁移 → `E_STATE`，原状态不变。
- 坏种子 → `E_SEED`，仅所属块失败，其他块保持提交。
- 预览分歧 → `E_DIVERGE`，给出三项实测值、首个分歧区域和最大差异像素。
- SQLite 哈希/版本被篡改 → 整快照作废并全量重算；独立验证器定位首个违规块、阶段和字段。

## 文档

- 架构、状态机、预算和验证口径：`docs/ARCHITECTURE.md`。
- 实际验证记录：`docs/VALIDATION.md`。
