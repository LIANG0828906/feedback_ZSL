# 实际验证记录

验证日期：2026-11-18  
环境：Linux/WSL2 x86_64、Python 3.11.18、NumPy 1.26.4、CPU 渲染、无 GPU/容器/外部资产。

## 复现命令

```bash
python -m gallery_louvers build-scene --out samples/scene.json
python -m gallery_louvers validate samples/scene.json
python -m gallery_louvers render samples/scene.json --out artifacts/delivery --fresh
python -m gallery_louvers render samples/scene.json --out artifacts/resumed_acceptance --fresh --stop-after-blocks 37
python -m gallery_louvers render samples/scene.json --out artifacts/resumed_acceptance
python -m gallery_louvers compare artifacts/delivery artifacts/resumed_acceptance --json artifacts/acceptance_diff.json
python -m unittest discover -v
```

## 交付样本

- 场景：`gallery-louver-array-v1`，4 层 × 16 片 = 64 片叶片，3 盏轨道射灯。
- 分辨率：480×360；分块 80×60；每角度 36 块；4 个角度；总计 144 块。
- 相机方位：-52°、-18°、18°、52°。
- 场景摘要：`50cb8223e6de056baf309f59bc7cfc1212fcfad2edb640375dcadcd610dfc0bd`。
- 花窗/拉丝图集摘要：`c40de3a8b025bfef3e74a7ba6f715a22eb4b69b843141af8135d1fa9f5fc53b9`。
- 生成摘要：`774b2a61a307ae595d31f0117b437be1972e8551090b148fa70d64c9cbeee34c`。
- 完成快照校验和：`9a7b1d8a83591c66872c3aeabd92c54a18a0c63f127ff24dff12c747c17531c2`。

参考图：

- `artifacts/delivery/renders/angle_000_rgba.png`（-52°）
- `artifacts/delivery/renders/angle_001_rgba.png`（-18°）
- `artifacts/delivery/renders/angle_002_rgba.png`（+18°）
- `artifacts/delivery/renders/angle_003_rgba.png`（+52°）
- `artifacts/delivery/diagnostics/wall_lightmap.png`
- `artifacts/delivery/diagnostics/floor_lightmap.png`
- 每盏灯单独光斑：`artifacts/delivery/diagnostics/*_lightmap_track-{L,C,R}.png`

## 验收结果

| 项目 | 实测结果 |
|---|---|
| 单元/集成测试 | 23 项通过，0 失败，日志见 `artifacts/acceptance_test.log` |
| 中断点 | 37 / 144 块 |
| 续算复用 | 37 块；随后新增 107 块并完成 144 块 |
| 中断/续算与不中断比较 | `identical=true`，`difference_counts={}`，退出码 0 |
| 线性 RGBA / 归属图 / PNG / 落点 / 光斑图 | 全部逐字节一致 |
| 真实 SIGINT 测试 | 完成当前块、提交快照、写恢复报告、退出码 0 |
| 真实 SIGKILL 测试 | 强制终止后仅凭已提交快照续算，与完整运行逐字节一致 |
| 场景/种子/快照损坏 | 均整体丢弃并从角度 0、块 0 重算 |
| 花窗重复生成 | 图集逐字节一致 |
| 光斑灰度重复运行 | 逐字节一致 |
| 多灯叠加顺序 | 正序/逆序逐位一致 |
| 三灯占比和误差 | 最大 `2.22e-16`（要求 ≤ `1e-6`） |
| 接收面总量约束 | 所有采样点 `total <= Σ single-light` |
| 半影距离扫描 | 两个控制变量扫描均为单调非减 |
| 高光截断占比 | 4 个角度均为 `0.0` |
| 同归属邻域孤立亮点/暗点 | 4 个角度均为 `0 / 0` |
| 背光花纹对比 | 0.164–0.175（4 个角度） |
| 同层相邻花窗最小平均绝对差 | 0.1767 |
| 光斑落点 | 1116 条，摘要 `1d42d1abe01559cd55787d477ece701baec1e74d3a94f114b4023638532f6ba2` |

## 逐层反射/透射

| 层 | 反射占比 | 透射占比 |
|---:|---:|---:|
| 0（inner） | 0.91058 | 0.08942 |
| 1（middle-a） | 0.90747 | 0.09253 |
| 2（middle-b） | 0.90825 | 0.09175 |
| 3（outer） | 0.90640 | 0.09360 |

## 中断与恢复

`artifacts/resumed_acceptance` 在第 37 块后收到停止请求，生成过 `recovery.json`；重新执行同一命令后快照校验通过，从第 38 块续算，最终全部图像与独立完整运行 `artifacts/delivery` 逐字节相同。完成运行会移除旧的 `recovery.json`。

保留的恢复报告样例：`artifacts/recovery_example.json`，来自 5 块后停止的独立探针；其中记录 `exit_code=0`、`current_block_completed=true`、下一块编号和快照校验和。

机器可读汇总：`artifacts/validation_summary.json`。

## 说明

- 所有位级对照都在同一 Python/NumPy/CPU 数值环境下完成。
- `artifacts/delivery` 与 `artifacts/resumed_acceptance` 各约 46 MB，包含可复算的分块、线性 RGBA、交接文档和诊断图。
- 未使用 Docker、GPU、模型/贴图下载或网络数据。
