# 离线空间声景渲染组件 — 技术说明

## 1. 范围与依赖

组件 `space_audio` 是纯离线、确定性的空间化 + 混响渲染链路，不访问网络、不依赖
外部音频素材或 HRTF 数据。运行依赖仅为 Python >= 3.10、NumPy >= 1.24、SciPy >= 1.10
（当前实现实际只用到 NumPy 与标准库，SciPy 为工程声明依赖）。

模块划分：

| 模块 | 职责 |
| --- | --- |
| `rng.py` | SplitMix64 确定性伪随机，种子与字符串/整数 token 混合，保证跨版本字节一致 |
| `wavio.py` | 纯标准库 IEEE-float32 WAV 读写，输出不依赖第三方编码行为 |
| `geometry.py` | 按种子 + 场景参数生成柱廊/拱门/铁栅栏实体，视线求交、遮挡布尔与绕射路径 |
| `synthesis.py` | 管风琴持续音、水滴脉冲、脚步瞬态三类干声的确定性合成 |
| `scene.py` | 场景 JSON 读取与结构化校验，输出字段路径 + 业务码 |
| `acoustics.py` | 单位能量混响 IR、overlap-save 分块卷积、解析 ITD/ILD 双耳模型 |
| `model.py` | 逐帧遮挡透射、绕射/距离衰减、混响区切换与 rt60 速率限制、方位角解包裹 |
| `renderer.py` | 固定采样块渲染、快照持久化、硬杀/优雅中断恢复、母带拼装 |
| `report.py` | 字段顺序稳定的空间音频报告、母带 SHA-256 与逐字节比对 |
| `cli.py` | `generate / render / compare / verify` 命令行 |

## 2. 场景与校验

`project` 声明采样率、控制帧率、块大小、时长、种子、距离/绕射衰减规则、混响变化
速率上限与过渡混合宽度；`sources` 给出唯一 ID、三维位置、干声文件、初始增益；
`path` 为时间升序的位置 + 朝向采样；`layers` 给出类型、平面 x、透射系数、混响参数
与几何参数。

校验在任何母带字节写出之前完成，错误聚合为 `{code, field_path, message}`：

- `DUPLICATE_SOURCE_ID`、`PATH_TIME_ORDER`、`TRANSMISSION_RANGE`
- `DRY_FILE_MISSING`、`SAMPLE_RATE_MISMATCH`
- `INVALID_SCENE`、`PARAM_RANGE`

CLI 以退出码 2 结束，且不创建工作目录/母带。

## 3. 确定性几何与干声

每层几何由 `seed_mix(scene_seed, "geometry", layer_id)` 驱动：

- 柱廊：等距圆柱 + 有界抖动；拱门：两侧立柱 + 顶部梁；铁栅栏：等距竖条。
- 视线段与层平面 `x = plane` 求交，交点落在实体内即遮挡；绕射路径长度取
  `2 * 到实体边缘的逃逸距离`。
- 每层独立求值、无共享状态，透射系数为各被穿越层系数的连乘，连乘可交换，
  因此与声源列表顺序无关。

干声由 `seed_mix(scene_seed, "dry", source_id, kind)` 驱动，重复生成逐字节一致。

## 4. 逐帧声学模型

- 透射：`T = Π transmission_i`（仅被穿越层计入）。
- 绕射衰减：`diffraction_db = -diffraction_db_per_m * detour_m`，
  幅度增益 `10 ** (db / 20)`。
- 距离衰减：`(distance_ref / distance) ** exponent`。
- 直达总增益 = 声源增益 × 透射 × 绕射增益 × 距离增益。
- 混响区：按听者 x 坐标相对层平面分区，边界 `crossfade_width` 内对相邻两层
  rt60/wet 线性混合；随后对 rt60 做速率限制
  `|rt60[k] - rt60[k-1]| <= max_rt60_rate_s / frame_rate`，保证混响时间不突变。
- 双耳：方位角先做跨帧 `unwrap`，再代入 Woodworth 球头 ITD
  `ITD = r/c * (θ + sinθ) * cosφ`，ILD 为 `8 dB * sinθ * cosφ`，
  左右耳增益由 ILD 换算；分数延迟用线性插值，逐采样更新，消除采样级断点。

## 5. 渲染与干湿能量

混响 IR 归一化为单位能量，湿声功率配比在渲染端按 `wet` 曲线施加
（`wet_amp = sqrt(wet)`、`dry_amp = sqrt(1 - wet)`），避免湿声被双耳增益二次放大。
每个声源对每个混响区持有一个 overlap-save 卷积器，块内对相邻区权重做线性交叉淡化；
卷积器历史与双耳 panner 历史均进入快照。

## 6. 快照与恢复语义

工作目录布局：`block_0000xx.raw`（float32 交错立体声块）、`snapshot.npz`、
`master.wav`、`report.json`，可选 `recovery_report.json` / `resume_mismatch.json`。

快照保存场景规范 JSON 的 SHA-256 摘要、采样率、块大小、总采样数、已完成块数、
恢复次数，以及全部卷积器/panner 状态。

- **强制终止（SIGKILL / `--kill-after-block`）**：仅已提交块 + 快照可信；重启只凭
  快照续算，不产生退出报告。
- **可处理中断（SIGINT/SIGTERM）**：信号仅置位，当前块完成、快照提交后写出
  `recovery_report.json`，退出码 0。
- **摘要不匹配**：场景摘要或关键工程参数变化时，删除全部旧块与快照，报
  `E_RESUME_MISMATCH` 并从零开始，绝不沿用旧块偏移或混响状态。
- 恢复后母带与一次性渲染逐字节一致；`compare` 子命令在不一致时输出块/采样/通道级
  差异并以退出码 3 结束（`MASTER_MISMATCH`）。

## 7. 报告稳定性

报告字段顺序固定，仅含相对文件名（`master.wav`），无时间戳、无绝对路径；
浮点数统一四舍五入。同一场景重复运行，除无耗时字段外完全相等，母带 SHA-256
随报告输出，可与 `compare` 结果交叉复核。

## 8. 常用命令

```bash
python -m space_audio.cli generate --scene demo/scene.json
python -m space_audio.cli render --scene demo/scene.json --work-dir demo/out_full --skip-generation
python -m space_audio.cli render --scene demo/scene.json --work-dir demo/out_kill \
    --skip-generation --kill-after-block 7   # 模拟硬杀，退出码 99
python -m space_audio.cli render --scene demo/scene.json --work-dir demo/out_kill --skip-generation
python -m space_audio.cli compare --reference demo/out_full/master.wav \
    --candidate demo/out_kill/master.wav
python -m space_audio.cli verify --scene demo/scene.json --report demo/out_full/report.json
python -m unittest discover -s tests
```

