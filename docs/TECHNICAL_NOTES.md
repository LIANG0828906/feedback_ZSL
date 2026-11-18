# 玫瑰窗离线软光栅验证组件 — 技术说明

## 1. 目标与范围

组件在没有外部模型、贴图或网络的前提下，从确定性 JSON 场景（`cathedral-rose/v1`）
渲染一扇直径 4 米、4 象限 × 5 扇形片的哥特式玫瑰窗，并输出可逐像素核对的分层图像、
铅条墙体投影落点、软阴影边缘测量、分块快照与稳定统计报告。纯 Python 3.11/3.12 实现，
仅依赖 NumPy 1.26 与 Pillow 10.2，不使用 Docker。

## 2. 模块结构

- `src/rosewin/scene.py`：由整数种子与三角函数确定性构造场景；`scene_digest` 用
  排序键、紧凑分隔的规范 JSON 计算 SHA-256，作为快照绑定的场景摘要。
- `src/rosewin/model.py`：结构化校验与类型化模型。重复象限、重复片、索引越界、
  退化三角形、透射率/反射率/铅条尺寸/颜色越界分别落到稳定业务码（如
  `E_INDEX_OUT_OF_RANGE`、`E_TRIANGLE_DEGENERATE`、`E_QUADRANT_DUP`、`E_FACET_DUP`）。
  校验先于任何像素计算，失败时只写错误 JSON 并以退出码 2 结束，不产生部分图像。
- `src/rosewin/rng.py`：splitmix64 与 FNV 风格整数哈希（均在 `errstate(over="ignore")`
  下做模 2^64 运算）。`fibre_texture_table` 以 `(scene_seed, stable_id)` 为键生成
  32×32 玻璃纤维斑驳表，值域 `[0.75, 1.0]`，周期双线性采样。纹理不使用全局随机状态，
  与片遍历顺序无关，重复生成逐字节一致。
- `src/rosewin/camera.py`：针孔相机，108° 垂直 FOV；每像素固定 2×2 子像素偏移，
  无未播种随机数。
- `src/rosewin/geometry.py`：向量化 Möller–Trumbore 求交、铅条边界距离、
  逐通道 Schlick Fresnel。
- `src/rosewin/rasterizer.py`：分块软光栅。每片对块内全部光线批量求交，最近 `t`
  获胜、等距时取最小 `stable_id`，因此多片命中的归属与输入列表顺序无关；分层输出
  背景、透射、釉面反射、铅条，并以 `sqrt` gamma 编码为 uint8。
- `src/rosewin/projection.py`：方向光几何投影、16 spp 分层抖动的墙体软阴影、
  落点坐标与随铅条深度增大的半影宽度测量。
- `src/rosewin/snapshot.py`：压缩 NPZ 快照，`os.replace` 原子整体替换；加载时校验
  场景摘要、数组形状与 JSON 完整性，任一不符即删除快照整体重算。
- `src/rosewin/pipeline.py`：分块调度、逐片统计、分层 PNG 与稳定字段顺序的报告。
- `src/rosewin/verify.py`：基线 → 可控中断 → 续算 → 逐文件逐像素结构化比对，
  并断言软阴影边缘宽度随深度单调。
- `src/rosewin/cli.py`：`generate-scene` / `render` / `verify-recovery` / `validate`。

## 3. 关键着色与几何决策

### 3.1 多波段透射，避免粉化与死黑

玻璃片位于 `z=0` 平面，外法线朝向室内相机（`-z`），室外太阳从 `+z` 侧入射。
逐通道 Schlick Fresnel：

```
R0(c) = glaze_reflectance * reflection_tint(c)
F(c)  = R0(c) + (1 - R0(c)) * (1 - cos_view)^5
T(c)  = transmission_rgb(c) * transmittance * mottle * (1 - F(c)) * incident(c)
```

`incident` 由方向光（含下限 0.18 的余弦项，保证侧向玻璃仍可辨认）与室内补光组成。
正对光源的蓝色玻璃因 Fresnel 与玻璃基色共同限幅，不会被加成粉白；侧向红玻璃保留
红通道主导。所有线性量在编码前 `clip` 到 `[0,1]`，uint8 通道永不溢出。

### 3.2 釉面反射与透射分离

反射层独立计算（ grazing Fresnel + Blinn 高光，受同一张纤维表调制），报告中
`transmission_ratio` 与 `reflection_ratio` 由各层线性能量占比给出且和为 1。
合成顺序固定为 `background + transmission + reflection`，再按铅条覆盖权重混入
铅条颜色，最后统一限幅；反射不会与透射简单相加越过图像上限。灰绿色调可通过
`reflection.png` 与报告中的反射占比直接定位到反射项。

### 3.3 铅条遮挡与墙体投影

每片铅条含两条径向边与内外两条圆弧，铅条按半宽参与屏幕空间软边遮挡。墙体投影把
窗平面点与下沉 `lead_depth` 的铅条尖端分别沿方向光投射到 `wall_z`：

```
hit = origin + light_dir * (wall_z - origin_z) / light_dir_z
```

报告同时给出 `source_xy`、`glass_plane_wall_xy` 与 `lead_wall_xy`，可直接核对投影
偏移。半影宽度 `2*(penumbra-umbra)` 随 `lead_depth` 线性增大，16 spp 抖动把边缘
转成连续灰度，消除硬黑边与锯齿；相同采样密度下抖动由种子决定，两次运行灰度分布一致。

### 3.4 分块、快照与恢复

图像按 32 行一块处理，每完成一块即把全量数组与块摘要原子写入快照。快照元数据含
场景 SHA-256、块数、块摘要与恢复次数。恢复只依赖已提交快照：摘要不一致或文件损坏
时丢弃全部旧块（不沿用旧行偏移/时间刻度），从零重算。中断模式在完成当前块、提交
快照并写出 `recovery_report.json` 后以退出码 0 结束，不写半成品 PNG。

### 3.5 报告稳定性

报告字段顺序固定，只含相对文件名、SHA-256 与数值统计，不含时间戳或绝对路径。
同一输入重复运行，除进程耗时外所有字段与全部 PNG/NPZ 字节一致。

## 4. 本地验证结果

使用 `.venv`（Python 3.12.16、NumPy 1.26.4、Pillow 10.2.0）执行：

- `python -m unittest discover -s tests`：19 个测试全部通过，覆盖校验业务码、
  纹理逐字节一致、分层不溢出、红蓝可辨、反射/透射占比、软阴影深度趋势、
  恢复逐字节一致、损坏/摘要不符快照丢弃。
- `verify-recovery`：中断 2 块后续算，6 张输出 PNG 与投影 JSON 全部
  `difference_count = 0`，`shadow_edge_width_trend = true`，退出码 0。
- 非法场景 6 类均返回结构化业务码与退出码 2，且无图像产出。

Windows 复跑入口见 `scripts/setup_windows.bat` 与 `scripts/acceptance_windows.bat`。
