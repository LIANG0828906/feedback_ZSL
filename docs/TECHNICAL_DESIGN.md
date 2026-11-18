# 陶窑玻璃砖墙确定性渲染器 — 技术说明

## 1. 范围与运行环境

- 纯 Python 3.11 + NumPy 1.26 实现，无 GPU、无网络、无外部素材；场景 JSON
  与全部玻璃砖墙三角网格由 `kiln_glass.scene.build_default_scene` 确定性构造。
- 本地验证环境：Python 3.11.18、NumPy 1.26.4（仓库 `.venv`）。
- 入口模块 `kiln_glass/cli.py`，子命令 `generate / validate / render / compare`。

```bash
uv venv --python 3.11 .venv && . .venv/bin/activate
uv pip install "numpy==1.26.4"
python -m kiln_glass generate --scene artifacts/scene.json
python -m kiln_glass validate --scene artifacts/scene.json
python -m kiln_glass render  --scene artifacts/scene.json --output artifacts/run_full
python -m kiln_glass compare --reference artifacts/run_full \
    --candidate artifacts/run_other --report diff.json --scene artifacts/scene.json
```

## 2. 模块结构

| 模块 | 职责 |
| --- | --- |
| `xrand.py` | SplitMix64 向量化哈希；所有随机量都是 `(seed, brick_id, channel, index)` 的纯函数 |
| `fringes.py` | 每块砖确定性应力条纹参数、气泡采样表、条纹调制与散射颜色 |
| `model.py` | 砖、层、点光源、接收面、相机、渲染配置的不可变数据结构 |
| `scene.py` | 确定性场景生成、JSON 读写、规范化字节（快照指纹） |
| `validate.py` | 11 类业务错误码的全量校验，错误含对象编号，校验失败即非零退出 |
| `geometry.py` | 相机光线、Möller–Trumbore 三角求交、AABB slab 求交、砖缝透视投影、半影模型 |
| `lighting.py` | 多火口软阴影、逐层 Beer–Lambert 透射、应力条纹散射、预乘 Alpha 合成 |
| `render.py` | 分块渲染、逐块快照、SIGINT/计划中断、恢复续算、报告汇总 |
| `snapshot.py` | 快照指纹、损坏/种子变更检测、失效即整体丢弃 |
| `band_report.py` | 亮带亮度剖面与半影宽度单调趋势样本 |
| `compare.py` | 两次输出的结构化逐像素/通道差异 |
| `io_utils.py` | 无依赖 PAM(RGBA)/归属图/JSON 的字节稳定读写 |

## 3. 需求对照实现

### 3.1 场景校验（业务要求 1）

`validate.validate_scene` 在任何渲染前执行，收集全部问题后再返回，错误码：

| 错误码 | 触发条件 |
| --- | --- |
| `E1001_MESH_INDEX_OUT_OF_RANGE` | 三角形顶点索引越界 |
| `E1002_DEGENERATE_TRIANGLE` | 三角形零面积/退化 |
| `E1003_THICKNESS_OUT_OF_RANGE` | 厚度不在 `(0, 1]` |
| `E1004_TRANSMITTANCE_OUT_OF_RANGE` | 透射率不在 `(0, 1]` |
| `E1005_BRICK_ID_DUPLICATE_OR_MISSING` | 砖编号重复、层引用缺失或覆盖不全 |
| `E1006_LAYER_ID_DUPLICATE_OR_MISSING` | 层序号非唯一、不连续或砖引用未知层 |
| `E1007_GROUT_WIDTH_NEGATIVE` | 砖缝宽度为负 |
| `E1008_VIEW_SEQUENCE_NOT_STRICTLY_INCREASING` | 视角序列非严格递增 |
| `E1009_LIGHT_INSIDE_BRICK` | 点光源落在某块砖 AABB 内部 |
| `E1010_INVALID_SCENE_PARAMETER` | 其余相机/接收面/配置类非法参数 |

CLI `validate`/`render` 校验失败返回退出码 `2`，输出含 `object_kind/object_id`
的结构化告警，不进入渲染。

### 3.2 条纹与气泡确定性（业务要求 2）

- `xrand.uniform(seed, brick_id, channel, index)` 仅依赖显式整数入参，不使用
  任何全局 RNG，与三角形遍历顺序无关。
- `fringes.generate_fringe_params(seed, brick_id, bubble_count)` 生成频率、相位、
  振幅、色相、散射增益与气泡表；同参两次调用数组逐字节一致，任一砖可独立复算。
- 条纹以砖面局部坐标 `(x, y)` 的多频正弦求值，调制 Beer 透射项与 HSV 散射色，
  相邻砖因 `(seed, brick_id)` 不同而深浅/色相可区分；气泡命中区域额外衰减 45%。

### 3.3 三视角自洽与逐字节一致（业务要求 3）

- 每个像素的最近砖命中由 AABB slab 求交确定，归属图只依赖场景几何与相机参数，
  `test_ownership_is_recomputable_from_scene` 仅凭场景描述重算并逐像素比对。
- 三角网格求交作为交叉校验（`mesh_owner_consistency`），默认每块抽样 32 条光线。
- 相机只绕声明 target 改变偏航角，砖墙几何与材质标识跨视角共享。
- RGBA 输出为确定性 PAM(P7)，同视角重复渲染逐字节一致（见
  `test_repeated_runs_byte_identical`）。

### 3.4 逐层透射与散射（业务要求 4）

- 入射角余弦 `cos θ` 影响有效厚度 `t/cos θ` 与入射因子；
  Beer–Lambert：`T = exp(-(1-τ)·t_eff·4)`。
- 透射再乘以条纹调制；增加环境条纹底光 `0.10 + 0.18·modulation`，使背对火口的
  外层砖仍可辨认条纹层次。
- 正对火口的砖通过 `clip(..., 0, 0.92)` 与软阴影衰减限制峰值，测试断言最大通道
  小于 245/255，不出现过曝惨白。
- 按层序号由内向外以预乘 Alpha `over` 算子合成，全部通道 `round` 后夹取到
  `[0,255]`，不溢出。

### 3.5 多火口层间投影与落点（业务要求 5）

- 砖缝线（层内砖边缘）由 `grout_gap_segments` 提取，`project_to_plane` 对每座
  火口做透视投影，分别落到外层砖墙与窑壁接收面（`grout_landings.json`）。
- 同一砖缝在三座火口下的落点坐标不同（测试断言三个光源落点集合互不相等）。
- 层间距/砖厚变化时整体 `plane_z` 与顶点同步移动，落点随之偏移
  （`test_landing_shifts_with_thickness_and_layer_distance`）。
- 落点超出接收面时标记 `in_bounds=false`，软阴影采样仅对实际命中遮挡体计遮挡，
  不会在边缘画硬黑边（`test_out_of_bounds_landing_is_flagged_without_hard_edge`）。

### 3.6 柔和亮带与半影单调趋势（业务要求 6）

- 软阴影使用固定黄金角螺旋圆盘样本 + 确定性哈希抖动，采样密度由场景
  `sample_density` 声明；同密度两次运行灰度分布逐字节一致。
- 半影宽度采用美术可控的有效光阑模型：
  `w = g · (d_lr / d_lg) · (1 + k·d_lg)`，其中 `d_lg` 为火口到砖缝距离、
  `d_lr` 为火口到接收面距离、`g` 为砖缝宽度；对两类距离均单调变宽。
- `band_trend.json` 输出每座火口/每层的落点、距离、宽度与 11 点亮度剖面，
  `verify_monotonic_trend` 分组断言单调性。

### 3.7 交叠角抗闪烁（业务要求 7）

- 像素按稳定的 `pixel_index` 生成抖动，砖处理顺序按砖序号排序，与片元遍历顺序
  无关；交叠处采用固定 Alpha 与确定性“近层（层号大、编号小）后合成”的规则。
- 同视角重复渲染逐像素一致，测试还扫描孤立极亮单点（与四邻域差异 >60 且 >180），
  断言数量为 0。
- `render_report.json/layer_contribution_ratios` 给出每层透射/散射能量占比，
  供美术核对碎点来源。

### 3.8 分块快照与强制终止续算（业务要求 8）

- 按声明 `tile_width/tile_height` 行主序分块；每完成一块立即写分块 PAM/归属图并
  覆盖同一份 `progress_snapshot.json`（原子 `os.replace`）。
- 快照含场景指纹（规范化 JSON + 全部条纹种子的 SHA-256）、场景摘要、已完成
  `view:tile` 键与每块颜色摘要（均值/极值/SHA-256）。
- 重启仅在指纹、摘要与 JSON 结构全部有效时续算；快照损坏、场景摘要不一致或条纹
  种子变更时丢弃快照，从首个视角重算，绝不沿用旧偏移。
- SIGINT（`--handle-sigint`）或计划中断（`--interrupt-after-tiles N`）都在块边界
  完成当前块、提交快照、写 `recovery_report.json`，退出码 0；强制终止不保证报告。
- 续算结果与不中断运行通过 `compare` 逐字节比对，不一致时输出含视角、分块、
  像素坐标与通道的结构化差异并以退出码 3 结束。

## 4. 输出产物

```
<output>/
  progress_snapshot.json        # 运行中持续覆盖，完成后删除
  recovery_report.json          # 优雅中断时写出
  grout_landings.json           # 每座火口的砖缝落点清单
  band_trend.json               # 亮度剖面与半影趋势
  render_report.json            # 层贡献占比、视角索引、重启标记
  view_000/...view_004/
    image.pam                   # 8-bit 预乘 Alpha RGBA
    ownership.kgown             # int32 逐像素砖编号（-1 为背景/砖缝）
    landing.json
```

## 5. 验证结果

测试与实测命令、耗时和结论见 `docs/VERIFICATION.md`。
