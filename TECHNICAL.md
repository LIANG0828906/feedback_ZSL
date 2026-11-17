# 技术说明

## 1. 模块边界

- `validation.py`：场景结构、体素字节、相机、光源、传输函数和采样配置校验。
- `transfer.py`：分段线性求值、边界 smoothstep 过渡、密度钳制和扫描表。
- `renderer.py`：相机光线、AABB 求交、三线性密度采样、分块光线步进和光照。
- `statistics.py`：收敛指标、可疑像素定位、光源贡献归一化。
- `snapshot.py`：块文件、原子清单、累计摘要、恢复和旧状态作废。
- `pipeline.py`：逐帧流程、最终产物、批量继续、全量/增量比较。
- `compare.py`：分块级差异、像素坐标和 RGBA/深度差值量级。
- `sample_data.py`：确定性体数据、传输函数、正常帧和坏帧生成器。

## 2. 输入校验与业务定位

校验先读原始字节，再解析 JSON 和体素。文件长度检查在 `frombuffer` 之前完成，
因此截断文件不会被短读后静默补零。NaN/Inf 检查和量程检查分别记录总数、首批
索引和值；非法输入不会进入渲染阶段。

传输函数校验顺序：

1. 每段 `start < end` 且端点和系数有限、非负。
2. 分段按起点单调，相邻区间不得重叠，也不得留间隙。
3. 第一段起点、最后一段终点必须闭合到声明量程。
4. 无过渡宽度时，左段终点与右段起点必须相同；有宽度时，宽度不得覆盖多个段。

错误以 `{code, scope, message, details}` 输出。批量模式捕获帧级拒绝，继续处理
后续帧，最终批次退出码为 `2`。

## 3. 传输函数与体积方程

密度先钳制到 `[density_min, density_max]`，越界样本在扫描表中同时记录
`input_density` 和 `clamped_density`。

每段内部对吸收系数和 RGB 自发光做线性插值。若
`transition_width = w > 0`，在内部边界 `b` 两侧 `[b-w/2, b+w/2]` 内，对左右
段函数做 smoothstep 混合。混合权重在区间两端分别退化为左、右段函数，因此在
扫描密度上连续。

采样方程：

```text
sigma_t(x) = rho(x) * absorption(TF(rho(x)))
j_self(x)  = rho(x) * emission(TF(rho(x)))
j_light(x) = sum_i rho(x) * light_i * attenuation_i(x) * phase_i(x)

T_{k+1} = T_k * exp(-sigma_t(x_k) * dt_k)
C      = sum_k T_k * (j_self(x_k) + j_light(x_k)) * dt_k
D      = sum_k sigma_t(x_k) * dt_k
alpha  = 1 - exp(-D)
```

点光源：

```text
attenuation = 1 / max(distance, min_distance)^2
phase       = HG(dot(ray_direction, direction_to_light), phase_g)
```

方向光不使用距离衰减，使用固定入射方向。光源按 id 排序后参与求和，避免 JSON
数组顺序造成浮点累加顺序变化。

## 4. 可复现规则

- 像素中心固定为 `x+0.5, y+0.5`。
- 每条光线从体积入口的固定相位开始，不依赖图像分块偏移。
- 最终统计不含墙钟时间；JSON 使用排序键、固定缩进和有限浮点。
- PNG 使用固定压缩参数和无时间戳文本元数据。
- 深度先量化为小端 `float32`，统计与可疑像素使用同一份输出精度。
- 帧哈希覆盖输入哈希、图像/深度哈希、采样规则、收敛结果、光源贡献、可疑
  列表哈希和完整块摘要。

## 5. 快照状态机

```text
fresh -> running -> completed
                 -> interrupted
running/interrupted + resume -> running -> completed
any state + input mismatch/corruption -> invalidated -> fresh
```

清单保存：

- 渲染器版本、输入指纹、场景 JSON 哈希、体素哈希；
- 分块布局及布局哈希；
- 已完成块的坐标、数组哈希、文件哈希、采样数、步长范围和分块贡献；
- 由已完成块重新计算的累计摘要。

块数组包含 RGBA、深度、光线区间、峰值密度/位置、采样体素包围范围。最终装配
只从磁盘重新读取这些块，而不是使用内存中的块结果，因此恢复路径与全量路径
共用同一份持久化契约。

输入或布局不匹配时，旧状态目录被整体移动，清单重新生成；新块 id 只来自当前
布局，旧行偏移不会参与。

## 6. 差异报告

`compare` 读取两边的 RGBA 和深度，按场景块尺寸分组。每个不一致块输出：

- `chunk_id`、块坐标和尺寸；
- 不一致像素数；
- 最大/平均 RGBA 差值；
- 最大/平均深度差值；
- 前若干个像素的坐标和逐通道差值。

同一块最多列出 `--max-samples-per-chunk` 个样本，同时用
`samples_truncated` 明确是否截断；总量和最大差值仍覆盖整块。

## 7. 本地验证记录

环境：

```text
Python 3.14.4
NumPy 2.5.5
Pillow 13.0.0
```

命令：

```bash
python -m unittest discover -s tests -v
```

结果：12/12 通过。覆盖：

- 9 类正常/坏帧业务码及整帧拒绝；
- NaN/Inf 精确定位；
- 传输函数钳制与边界连续性；
- 收敛通过和不通过两条路径；
- RGBA/深度/统计/扫描表/可疑清单字节复现；
- 分块尺寸变化和光源顺序变化不改变像素结果；
- 批量坏帧继续；
- 真实 SIGKILL 后续算与全量一致；
- SIGTERM 返回 143、恢复报告和续算；
- 快照输入/布局变化整体作废；
- 差异报告中的块号、像素坐标和差值量级。

