# 异常注入配置（参数/输入驱动，禁止修改源码）

注入通过 `SegmentationParameters(injections=...)` 或 CLI `--inject 文件.json`
传入。支持的键：

## E_SEED — 种子落在背景

```json
{ "seed_background": { "blocks": ["b00"] } }
```

同时需要把种子点移到背景，例如 CLI 参数对应
`SegmentationParameters(seed_points={"b00": (4, 4)})`。

## E_SPILL — 容差过大导致外溢

```json
{ "spill_tolerance": { "blocks": ["b01"] } }
```

配合 `color_tol=260`、`gradient_tol=400`，生长面积超过
`spill_area_ratio` 倍真值面积即触发。

## E_REGION — 区域数量异常

- 输入文件驱动：删除清单中某个区域（见 `examples/run_demo.py` 第 5 节），
  加载期返回 `E_META` 并列出缺失块；
- 连通域驱动：把 `min_region_pixels` 调到极大（如 `10^9`），stats 阶段
  连通域数量归零，返回 `E_REGION` 并给出 missing/extra 编号。

## E_BLOCK — 坏块

```json
{ "bad_block": { "blocks": ["b03"], "mode": "corrupt", "seed": 777 } }
```

`mode` 取值：`corrupt`（确定性随机损坏）、`zero`（全零块）、
`hash_break`（首字节翻转，配合块哈希基线）。
