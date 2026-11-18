# 绣楼戏台过场：确定性离屏渲染器

纯 Python（NumPy + Pillow）实现的民国题材绣楼戏台过场渲染器，在
1920×1080、60° 视场角下离屏渲染“三层缎面绣幕 + 两层缂丝隔断”交叠遮挡
结构。全部网格、材质采样表、孔洞、相机路径与灯光均由固定整数种子确定性
构造，不依赖任何外部模型、纹理、图像或网络资源。

## 1. 环境与安装

- Windows 10/11、Linux 或 macOS，Python 3.10+（开发与验证环境为 Python 3.14）。
- 安装依赖：`pip install -r requirements.txt`（仅 `numpy`、`Pillow`）。
- 不使用 Docker 或其他容器；所有验证均为本地确定性样本。

## 2. 目录结构

| 模块 | 职责 |
| --- | --- |
| `brocade_embroidery/rng.py` | splitmix64 整数哈希、与全局 RNG 状态无关的确定性随机原语 |
| `brocade_embroidery/geometry.py` | 向量、平面求交、重心坐标、透视相机与环绕路径 |
| `brocade_embroidery/scene_gen.py` | 固定种子生成五层网格、材质、双点光源、地面与相机路径 |
| `brocade_embroidery/validation.py` | 业务约束校验与结构化错误码 |
| `brocade_embroidery/sampling.py` | 缎面绣纹表与缂丝孔洞表（仅凭种子/层号/分辨率复算） |
| `brocade_embroidery/shadow.py` | 孔洞透视投影、落点清单、随距离单调变宽的高斯半影 |
| `brocade_embroidery/shading.py` | 各向异性 Ward 高光、包裹漫反射、缂丝透射、空气隙衰减 |
| `brocade_embroidery/rasterizer.py` | 分块软件栅格化、深度归属、预乘 Alpha 合成、稳定统计 |
| `brocade_embroidery/snapshot.py` | 原子快照、场景/种子摘要校验、损坏整体丢弃 |
| `brocade_embroidery/render.py` | 渲染编排、SIGINT 优雅退出、图像/归属图/报告落盘 |
| `brocade_embroidery/acceptance.py` | 高光连续性、半影趋势、孤立亮点、强制恢复一致性验收 |
| `brocade_embroidery/cli.py` | 命令行入口 |
| `tests/` | 24 个 `unittest` 用例 |
| `scripts/` | 演示与真实 SIGKILL 恢复脚本 |

## 3. 常用命令

```bash
# 生成确定性场景（小场景用于快速验证；去掉 --small 即 1920×1080 正式场景）
python3 -m brocade_embroidery.cli generate-scene --scene scene.json --small

# 严格校验，失败时退出码为 2 并输出带构件编号与错误码的 JSON
python3 -m brocade_embroidery.cli validate-scene --scene scene.json --report validation.json

# 渲染某一环绕角度；默认按快照续算，--fresh 强制从零开始
python3 -m brocade_embroidery.cli render --scene scene.json --output out --frame 0

# 完整验收（高光、半影趋势、孤立亮点、硬终止恢复逐字节一致性）
python3 -m brocade_embroidery.cli acceptance --output acceptance --small

# 真实 SIGKILL 后仅凭快照恢复并逐字节比对
bash scripts/force_kill_check.sh

# 运行全部单元测试
python3 -m unittest discover -s tests
```

正式 1920×1080 场景（每层 3584 三角面、五层共 17920 面、64 个 240×135 块）
单帧在参考机器上约 35 秒。

## 4. 业务要求与实现映射

### 4.1 几何与材质校验（要求 1）

`validation.validate_scene` 输出结构化告警列表，每条包含 `error_code`、
`component_id`、`message` 与可选 `detail`。覆盖：

- 网格索引越界 `E-MESH-INDEX-OUT-OF-BOUNDS`、退化三角形 `E-MESH-DEGENERATE`；
- 反射率/粗糙度/透影率/孔洞率越界（`E-MATERIAL-*`，统一限定开区间 (0,1)）；
- 层序号或构件编号重复/缺失（`E-LAYER-DUPLICATE`、`E-COMPONENT-DUPLICATE` 等）；
- 点光源落在任一绣幕平面内 `E-LIGHT-IN-LAYER-PLANE`（距离阈值 1e-5）；
- 相机、分块、地面与灯光数量/颜色/强度合法性。

任一告警都会让 CLI 以非零整数退出码结束，且不会进入渲染。

### 4.2 确定性采样表（要求 2）

- 所有随机量来自 `rng.hash_mix(seed, layer, resolution, row, col, ...)` 的
  splitmix64 哈希，不调用未播种随机数，也不依赖三角形遍历顺序。
- `build_embroidery_table` 输出绣纹调制、丝线角度与各向异性缩放三张
  little-endian float32 表；`build_hole_table` 输出软边孔洞覆盖表。
- 表仅由“整数种子 + 层序号 + 分辨率”决定，重复生成 `to_bytes()` 逐字节一致。
- 绣纹调制各向异性反射率与高光切线方向；孔洞调制透影率，配合不同基色让
  同色系相邻层仍可区分。

### 4.3 各向异性着色与合成（要求 3）

- 缎面使用各向异性 Ward 高光，切线沿绣线方向并受绣纹角度场扰动；粗糙度设
  下限、高光设上限（`SPECULAR_CEIL`），斜视角不会拉出刺眼亮带。
- 漫反射采用 wrap lighting 并叠加环境光与背面补偿，背光面仍保留绣纹明暗
  层次，不会整片死黑。
- 缂丝隔断按孔洞覆盖与透影率累积透射光，仅保留很弱的近各向同性丝光，孔洞
  本身不产生高光闪烁。
- 由内向外以预乘 Alpha over 算子合成 RGBA；逐像素构件归属图按相机空间最近
  深度写入，独立于混合顺序，可仅凭场景描述复算。

### 4.4 孔洞层间透影（要求 4）

每个缂丝孔洞沿每盏点光源方向做光线-平面求交，分别投影到所有更外层绣幕与
戏台地面：

- 同一孔洞在两盏灯下产生位置不同、可区分的重叠光影；
- 报告 `report.json` 的 `landing_points` 给出每盏灯的世界坐标、接收面 UV、
  是否落在接收面内与半影宽度；
- 超出接收面的落点按无遮挡处理（不写入接收场），因此不会在边缘形成硬黑边；
- 接收面或层间距移动时，落点随之偏移（有专门单元测试）。

### 4.5 柔和半影与趋势（要求 5）

半影宽度采用风格化但单调的闭式公式：

```
w = (0.035 + 0.010 * 灯到缂丝距离 + 0.028 * 缂丝到接收面距离) / sqrt(采样密度)
```

它随两个距离单调变宽、随采样密度提高而变窄；落点用无阈值高斯斑叠加，天然
无锯齿硬边。验收报告 `penumbra_trend` 给出 3×3 距离组合的宽度比较，
`shadow_distribution` 证明同采样密度两次重建的透影灰度逐字节一致。

### 4.6 交叠亮斑/暗点抑制（要求 6）

- 栅格化按固定三角形数组与固定分块顺序处理，深度比较用确定性浮点，平局由
  固定顺序决定，不随相机或遍历顺序产生孤立跳变。
- 空气隙厚度与入射角通过 `inter_layer_attenuation` 连续衰减层间透射累计。
- 验收以四邻域亮度差统计孤立像素，参考场景结果为 0；报告 `layer_contribution`
  输出每层缎面反射与缂丝透影占比，供美术判断层序。

### 4.7 分块快照与恢复（要求 7、8）

- 按声明块尺寸逐块渲染，每块通过临时文件 + `os.replace` 原子整体替换
  `snapshot.json`，并记录已完成块编号、场景摘要（SHA-256）、种子签名与每块
  颜色摘要。
- 重启时校验场景摘要、帧参数、绣纹/孔洞种子签名及块 blob；任一不一致或
  损坏即整体清空并从第 0 块重算，绝不沿用旧块偏移。
- SIGINT：安装信号处理器，完成当前块、提交快照、写 `recovery.json` 后以
  退出码 0 结束；SIGKILL：不保证报告，下次仅凭已持久化快照恢复。
- 续算后的 RGBA PNG、uint16 归属图与透影落点坐标与不中断运行逐字节一致；
  不一致时 `compare-images` 输出含像素坐标与差异通道的结构化差异并以退出码
  5 结束。
- `stable_statistics` 给出每块可见片元数、平均反射率、透影采样点数与快照
  提交次数。

## 5. 实测验证结果

在本工作区执行并通过：

- `python3 -m unittest discover -s tests`：24 个用例全部通过，覆盖校验错误码、
  采样字节一致性、高光连续性、背光层次、半影单调、落点随几何偏移、归属图
  复算、SIGINT、快照损坏/种子变更丢弃、逐字节恢复等。
- `bash scripts/force_kill_check.sh`：真实 SIGKILL 后重启，RGBA PNG 与
  uint16 归属图逐字节一致，4168 个透影落点坐标完全一致。
- `acceptance_report.json`：`png_bytes_identical=true`、
  `shadow byte_identical=true`、`isolated_pixel_count=0`，高光最大邻域通道
  跳变约 0.12（远低于刺眼阈值）。

## 6. 输出物说明

每个帧目录包含：

- `render.rgba.png`：预乘 Alpha 的 RGBA 图像；
- `ownership.png`（8 位预览）、`ownership.u16`（权威 uint16 归属图）、
  `ownership.png.json`（构件编号映射）；
- `report.json`：分块统计、层贡献占比、透影落点清单、恢复信息；
- `recovery.json`：中断/恢复状态；
- `snapshot/`：原子快照与每块 float32 RGBA / uint16 归属 blob。

## 7. 设计取舍

- 半影公式以业务单调性为准（物理软阴影通常与光源尺寸/遮挡物距离相关），
  在技术说明中明确标注为风格化模型，便于美术直接调参。
- 软件栅格化以固定遍历换取可证明的确定性，而非 GPU 并发吞吐；正式分辨率
  单帧约 35 秒，可通过增大块尺寸或向量化更多内层循环进一步提速。
- 归属图权威数据使用原始 uint16（`.u16`），PNG 仅作 8 位可视化预览，规避
  不同 Pillow 构建对 16 位 PNG 模式的兼容差异。
