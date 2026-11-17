# 柳编斗拱灯箱 · 离线软光栅渲染验证组件

从 JSON 场景描述确定性复算五层柳条编织灯箱的 RGBA 图像、逐像素层归属图与
三盏油灯的孔洞投影落点清单，并输出稳定统计报告。纯 CPU、无 GPU、无网络依赖，
仅需要 Python 3.11+、NumPy、Pillow。

## 快速开始

```bash
pip install numpy pillow          # 或 uv pip install numpy pillow

# 生成确定性样本场景（同一种子逐字节一致）
python -m willow_renderer gen-sample --out scene.json --seed 20261118

# 渲染（分块 + 快照 + 报告）
python -m willow_renderer render --scene scene.json --outdir out

# 与参考输出逐像素核对（不一致时输出结构化差异并以退出码 4 结束）
python -m willow_renderer render --scene scene.json --outdir out2 \
    --expect-rgba out/rgba.png --expect-owner out/owner.png \
    --expect-landings out/landings.json

# 运行测试（stdlib unittest，无需 pytest）
python -m unittest discover -s tests -v
```

## 输出物（`--outdir`）

- `rgba.png` — 按层序号由内向外合成的预乘 Alpha RGBA 图像，通道钳制在 [0,1]。
- `owner.png` — 逐像素层归属图（单通道：0..4 层序号，254 内壁，255 背景；
  同一像素多层重叠时取最外可见层）。`owner_preview.png` 为调色板预览。
- `landings.json` — 每盏灯 × 每个孔洞 × 每个接收面的投影落点坐标清单
  （含 `in_range`、空气隙、灯距）。
- `report.json` / `report.txt` — 稳定统计报告：各层透射/反射贡献占比、
  半影采样点亮度与边缘过渡宽度、输出文件 SHA-256 摘要。
- `_snapshot.json` / `_partial_*.npy` — 渲染期间的恢复快照（完成后自动清除）。
- `recovery_report.json` — 可处理中断（SIGINT/SIGTERM）时写出的恢复报告。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功（含可处理中断：完成当前块、提交快照、写恢复报告） |
| 1 | 未预期内部错误 |
| 2 | 场景校验错误（stderr 输出含 `layer_id` 与字段路径的结构化错误码） |
| 3 | 场景文件 IO / JSON 解析错误 |
| 4 | `--expect-*` 参考核对不一致（含分块编号、像素坐标、差异通道） |
| 5 | `--strict-snapshot` 模式下快照损坏 |

校验错误码：`MESH_INDEX_OUT_OF_RANGE`、`TRIANGLE_DEGENERATE`、
`TRANSMITTANCE_OUT_OF_RANGE`、`DUPLICATE_LAYER_ID`、`DUPLICATE_HOLE_ID`、
`LAMP_INSIDE_GEOMETRY`、`AIR_GAP_INCONSISTENT`、`CAMERA_INVALID` 等。

## 设计要点

- **确定性**：全部 float64 固定求值顺序；每层纹理由各自整数种子经 PCG64
  生成编织纹理采样表与孔洞掩码表，与层遍历顺序无关；三角形按场景文件顺序
  光栅化，z-buffer 严格小于更新，交叠片元结果与遍历顺序无关，两次运行逐
  字节一致。
- **着色**：每片元按入射角累积三盏灯的透射光（经内层透光率衰减 + 孔洞
  投影光场）与纹理调制的漫反射；`x/(1+x)` 软色调映射避免内层惨白过曝，
  透射光经染色色板着色保持外层染色层次；由内向外按预乘 Alpha over 合成。
- **孔洞投影**：孔洞单元四边形以灯位为中心透视映射到相邻外层与内壁平面；
  落点超出接收面范围时按无遮挡处理（不生成光斑也不产生边缘硬黑边）；
  边缘按 `projection.edge_sample_density` 超采样柔和过渡，半影宽度
  `softness * (空气隙 + 灯到孔洞层距离)` 随两者增大而变宽，报告给出隔离
  光斑上亚像素插值测得的 10%–90% 过渡宽度以体现该趋势。
- **分块快照**：每完成一块以临时文件 + `os.replace` 整体替换方式原子提交
  （已完成块编号、场景内容摘要、各块颜色摘要、部分缓冲摘要）。重启时任一
  校验失败即整体丢弃从零重算，绝不沿用旧快照的块偏移或纹理采样值；续算
  结果与不中断运行逐字节一致。
- **中断**：SIGINT/SIGTERM 置标志，当前块完成并提交后写
  `recovery_report.json` 以退出码 0 结束；SIGKILL 等强制终止仅凭已提交
  快照恢复。`WILLOW_TILE_DELAY_MS` 环境变量用于测试中拉宽中断窗口。

## 实际验证结果

- `python -m unittest discover -s tests -v`：17 项测试全部通过，覆盖
  六类校验错误退出码、两次运行逐字节一致、中断+续算与一次性渲染逐字节
  一致、快照损坏（默认丢弃重算 / strict 退出码 5）、场景摘要不一致丢弃、
  参考核对退出码 4、半影宽度随空气隙与灯距增大的趋势。
