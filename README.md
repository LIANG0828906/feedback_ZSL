# VSRender：哈希绑定的本地体渲染组件

本项目面向“预览、离屏渲染、快照三者必须严格对齐”的桌面体渲染场景。所有样本由确定性生成器在本地产生，不依赖网络、账号或真实医学影像。

核心约束是：**任何一次可见画面都必须绑定唯一一组已提交参数；参数数值、参数哈希、颜色缓冲和快照必须能相互校验。**

## 安装

要求 Python 3.10 或更高版本。

```bash
python -m pip install -r requirements.txt
```

依赖：NumPy、PyQt5、PyOpenGL。核心渲染器采用 NumPy CPU 光线投射，以保证同参数同视角的字节级确定性；PyOpenGL 用于示例程序启动时探测本地离屏 OpenGL 上下文，不参与颜色缓冲的真值生成。

## 快速开始

```bash
# 生成 64^3、128^3 两级样本、基线参数、颜色缓冲、快照和轨迹
python -m volrender.cli generate --root output --seed 19093 --sizes 64 128

# 运行固定的窗宽窗位/TF/掩码/快照操作序列
python -m volrender.cli demo --root output --size 64

# 执行完整验收并输出报告
python -m volrender.cli verify --root output

# 启动桌面示例
python -m volrender.cli gui --root output --size 64
```

Windows 下同样使用上述命令。无显示环境可用 `QT_QPA_PLATFORM=offscreen` 做 GUI 烟测。

主要输出：

```text
output/
  samples/      两级 .vol 样本与 samples.json
  params/       基线不可变参数组
  buffers/      .rgba 颜色缓冲、.json 缓冲清单、可选 .mask 覆盖轨迹
  snapshots/    原子写入的参数组快照
  traces/       确定性 JSONL 轨迹
  reports/      基线与验收报告
```

## 二进制体数据格式

头固定 128 字节，小端序。标量类型 `2 = uint16`、`4 = float32`；掩码固定 `uint8`。

| 偏移 | 字段 | 类型 | 说明 |
| ---: | --- | --- | --- |
| 0 | magic | 8 bytes | `VSRENDER` |
| 8 | version | u16 | 当前为 1 |
| 10 | header_size | u16 | 128 |
| 12 | scalar_type | u16 | 2 或 4 |
| 14 | flags | u16 | bit0 表示存在掩码 |
| 16 | dims | u32 × 3 | nx, ny, nz |
| 28 | spacing | f32 × 3 | sx, sy, sz，必须为正 |
| 40 | window_center | f32 | 窗位 |
| 44 | window_width | f32 | 窗宽，必须为正 |
| 48 | density_offset | u64 | 固定为 128 |
| 56 | density_bytes | u64 | 必须等于维数乘积 × 标量字节数 |
| 64 | mask_offset | u64 | 掩码起始偏移 |
| 72 | mask_bytes | u64 | 必须等于掩码维数乘积 |
| 80 | mask_dims | u32 × 3 | 必须与密度维数一致 |
| 92 | voxel_count | u64 | 必须等于密度维数乘积 |
| 100-127 | reserved | - | 预留 |

加载时会校验头部字段自洽、文件总长与声明范围一致、密度/掩码长度正确、间距和窗宽为正。密度字段问题返回 `E_HEADER_INVALID`；掩码维度或偏移问题返回 `E_MASK_INVALID`，异常对象包含 `field`、期望值、实际值等定位信息。

窗口映射：

```text
low  = center - width / 2
high = center + width / 2
mapped = clip((raw - low) / width, 0, 1)
```

`WindowReference` 记录 `center/width/raw_low/raw_high/mapped_min/mapped_max/mapped_mean`。改变窗宽窗位后，`compare_window_mappings` 在全体体素上重算并返回最大绝对偏差与受影响体素数。

## 参数组与内容哈希

`RenderParameters` 是不可变数据对象。参数哈希是对规范化 JSON 内容做 SHA-256，覆盖：

- 体数据内容哈希；
- 窗位、窗宽（浮点十六进制，避免格式化歧义）；
- 全部传递函数控制点（位置、不透明度、RGB）；
- 采样步长；
- 视角矩阵、视口尺寸；
- 掩码启用状态与掩码内容哈希。

因此窗宽窗位、控制点序列、采样步长任一项不同，参数哈希必然不同。传递函数使用分段线性插值；控制点位置必须严格递增且位于 `[0,1]`。非法更新返回 `E_TF_INVALID`，且编辑器保留上一有效 LUT。

## 离屏渲染

`render_offscreen(volume, params)` 的固定链路：

1. 按参数组窗口映射密度场；
2. 构建分段线性 RGBA LUT 并查表；
3. 三线性插值密度、梯度和梯度光照；
4. 沿光线前向 alpha 合成；
5. 输出直通 alpha（straight-alpha）RGBA8 缓冲。

每次渲染通过光线行批次检查取消令牌。同一参数组、同一视角重复渲染必须得到完全相同的字节和 SHA-256。

颜色缓冲发布时生成清单，至少包含参数哈希、缓冲内容哈希、视口、掩码状态和完整参数组。读取时重新计算内容哈希；参数哈希缺失、参数哈希与清单参数不一致、缓冲内容被篡改均返回 `E_PARAM_STALE`。文件时间、文件大小不能代替哈希作为真值来源。

## 预览状态机

`PreviewController` 强制以下行为：

- `commit(params, result)` 校验参数哈希、缓冲绑定和视口一致后才替换可见帧；
- 拖动期间只调用 `set_pending(new_params)`，当前画面、数值读数和哈希仍来自上一已提交参数组；
- 界面状态同时暴露 `displayed_parameter_hash` 与 `pending_parameter_hash`，并标记 `pending_is_uncommitted`；
- 提交新结果后才切换画面；
- 若显示的参数与所绘缓冲绑定不一致，返回 `E_PREVIEW_MISMATCH` 并定位首个不同字段。

示例界面把“显示中已提交参数”和“未提交待渲染参数”分成两行显示，避免用滑块当前值冒充画面参数。

## 掩码一致性

掩码与密度场使用同一参数组、同一光线位置。掩码标记样本以低权重红色半透明叠入同一前向合成链，并记录每像素是否经过掩码体素的覆盖轨迹。

验证会分别渲染“无掩码”和“有掩码”结果，检查：

```text
changed_pixels 必须是 mask_coverage 的子集
changed_pixels & ~mask_coverage == empty
```

掩码维数、偏移、长度或哈希不符返回 `E_MASK_INVALID`。缓冲清单保存覆盖轨迹哈希，载入时同颜色缓冲一起校验。

## 快照与恢复

快照为版本化 JSON，记录：

- 完整参数组与参数哈希；
- 视角矩阵、视口、采样步长；
- 掩码状态/哈希；
- 颜色缓冲内容哈希与缓冲名；
- 掩码覆盖轨迹哈希；
- 由全部字段计算的 `snapshot_id`。

保存流程使用同目录临时文件、`fsync` 和 `os.replace` 原子重命名。载入时校验 schema、版本、参数哈希、字段冗余一致性和 `snapshot_id`。损坏或字段缺失返回 `E_SNAPSHOT_INVALID`，且载入路径不写入任何输出。

`SnapshotManager.restore` 仅在参数、视角、视口、步长、掩码和缓冲哈希全部匹配且颜色缓冲重新验真通过时复用旧缓冲；任一字段不同都会重新渲染并发布新缓冲。强制终止后的 `recover_from_snapshot` 只以磁盘快照为输入，不生成退出报告。

## 并发与取消

`OffscreenRenderService` 以体数据键做单飞控制：

- 同一体数据已有渲染任务时，重复提交返回 `E_BUSY`；
- 取消采用协作式事件，任务在下一次光线批次检查点抛出 `RenderCancelled`；
- 取消或异常发生在提交前，离屏数组只在任务栈内，文件句柄由上下文管理器关闭；
- 失败任务不会覆盖最后一次已提交快照；
- 不提供“强制终止后自动生成报告”的承诺，恢复只依赖磁盘快照。

## 业务错误码

| 错误码 | 触发场景 |
| --- | --- |
| `E_HEADER_INVALID` | 头字段矛盾、维数/间距/窗宽非法、密度长度不符 |
| `E_TF_INVALID` | 控制点越界、非严格递增、数量不足、LUT 尺寸非法 |
| `E_PARAM_STALE` | 参数哈希缺失、缓冲清单与内容/参数不符、请求了其他参数组 |
| `E_PREVIEW_MISMATCH` | 显示参数哈希与所绘缓冲绑定不一致 |
| `E_SNAPSHOT_INVALID` | 快照版本/schema/哈希/字段缺失或损坏 |
| `E_BUSY` | 同一体数据已有离屏渲染任务 |
| `E_MASK_INVALID` | 掩码维数、偏移、长度、哈希或叠加绑定非法 |

## 测试与实测结果

```bash
python -m unittest discover -v
python -m volrender.cli verify --root output
```

当前环境实测：

- 单元测试：33 项全部通过；
- 验收检查：25 项全部通过，0 失败；
- PyQt5 离屏 GUI 烟测：退出码 0，PyOpenGL 离屏上下文可用；
- 64³ 与 128³ 基线参数哈希、颜色缓冲哈希见 `output/reports/baseline.json`；
- 相同种子重复生成：体数据文件、参数哈希、颜色缓冲、轨迹日志逐字节一致。

验收明细见 `output/reports/verification.json` 与 `output/reports/verification.md`；更细的架构和需求映射见 `docs/DESIGN.md`。

## 确定性边界

同一台机器、同一 NumPy/OpenBLAS 构建下，本路径使用固定采样顺序、固定批量边界和确定性浮点数据处理。跨 CPU 架构或不同数学库构建的字节级一致性不属于承诺；如需跨平台逐字节一致，应在交付侧固定运行镜像和依赖版本（本项目约束不引入容器）。
