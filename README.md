本地离线渲染工具需要为含烟尘与玻璃罩的场景生成带体积光散射的预览图：场景由自定义二进制描述提供（相机内外参、若干轴对齐盒与球体透明介质、方向光、介质密度场分块索引），渲染管线依次执行光线求交、介质边界穿越与分段、沿视线在介质内的步进采样、散射累积、透射率与阴影射线可见性估计，最终输出颜色与误差摘要。难点在于并行分块与资源预算的双重约束：屏幕被划分为分块渲染任务，任务跨分块可并行但每一分块内步进顺序固定，全局并行度与单任务采样预算来自配置；预算不足时必须降级步长或拒绝启动，而分块边界处相邻光线的散射累积与透射率必须保持连续，否则会出现可见接缝。

任务环境与准备：
安装 Python 3.11，并通过 pip 安装 NumPy 与 PyOpenGL，且可在 Windows 上打开 OpenGL 窗口（本地驱动即可）。
在 Windows 本地工作区创建项目根目录，确认无需网络账户、付费资源或外部数据集。
准备由题目内样本生成器产出的场景二进制、密度场分块索引与基线哈希，测试数据全部本地生成。
运行脚本化入口时使用本地回环服务与临时目录，避免依赖线上站点或容器环境。

业务要求：
1. 实现场景二进制解析与介质几何构建：读取相机内外参、轴对齐盒与球体透明介质、方向光与密度场分块索引，校验矩阵可逆性、介质嵌套关系与密度索引范围；字段缺失、几何退化或索引越界返回业务码 E_SCENE_INVALID 并定位字段路径。
2. 实现分块渲染调度器：把屏幕按配置分块尺寸切分为渲染任务，任务之间可并行但同一分块内步进采样顺序固定，可运行任务按提交序号稳定排序；全局并行度与单任务采样预算来自配置文件，超预算任务进入等待队列，调度决策写入轨迹，重复启动整体渲染返回 E_BUSY。
3. 实现介质内步进采样与散射累积：沿视线穿越介质边界得到分段，按步长自适应采样密度场，累积散射与透射率并做阴影射线可见性估计；单任务采样数累计超出其预算时不得启动或须按降级策略增大步长，拒绝启动返回 E_BUDGET_EXCEEDED 并保留已有提交结果。
4. 实现分块边界的散射连续性与拼接：相邻分块共享边界像素的采样相位，拼接处散射累积与透射率偏差须在声明容差内；以整屏单块渲染结果为真值来源比对分块结果，超差时返回 E_SEAM_UNRESOLVED 并定位接缝像素与所在分块对。
5. 实现精度模式与误差摘要：提供浮点与定点两条散射累积路径，切换后重新渲染并输出两种模式的逐像素偏差统计与误差上界；每次渲染输出含分块层级、预算用量、步长与误差摘要的确定性摘要，固定视角与参数下连续两次渲染缓冲区逐字节一致且不得依赖系统时间。
6. 实现增量重渲染与依赖失效：相机、光源、介质参数或密度场某分块变化后，计算受影响屏幕分块集合并只重渲该集合，未受影响分块复用缓存结果与内容哈希；无法证明未受影响时保守重渲，重渲完成后以整体替换发布新版本，替换前预览持续显示旧版本。
7. 实现原子快照与强制终止恢复：把分块表、每块结果内容哈希、已完成分块索引、密度版本、预算配置与精度模式写入快照，保存采用临时文件加原子重命名；恢复时校验版本与哈希链，从最近有效快照继续且不重复写入已完成分块，断链或字段缺失以 E_RECOVERY_INVALID 中止。
8. 实现图形预览界面与确定性样本入口：界面展示渲染图、分块网格、已完成分块着色与预算占用曲线，支持调节分块尺寸、并行度、预算与精度模式并协作式取消；提供样本生成器产出多种分辨率、介质数与密度分布的场景及基线哈希，输出含分块、采样数、预算用量、误差与哈希的轨迹日志。

执行约束：不使用 Docker 或其他容器环境；使用本地确定性样本验证，不访问未授权目标。按上述任务意图交付相应代码、测试或技术说明，记录实际验证结果。

验收：
用本地样本验证分块渲染、预算降级、接缝连续、增量重渲与快照恢复，并附轨迹日志。

---

# 实现说明

本仓库已提供完整的本地离线实现，入口为 `dustrender` Python 包。实现不使用容器、
在线账户、付费资源或外部数据集。场景、密度块、基线哈希、轨迹与验收报告均由本地
确定性流程生成。

## 环境

- Python 3.11
- NumPy 2.4.6
- PyOpenGL 3.1.10
- 图形界面使用 Python 自带 `tkinter`；独立 OpenGL 预览使用 PyOpenGL/GLUT 和本地
  图形驱动

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -U pip
.\.venv\Scripts\python -m pip install -e ".[dev]"
```

Linux/macOS：

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -e ".[dev]"
```

无需设置网络服务地址。脚本化服务只绑定 `127.0.0.1`，渲染输出写入显式指定目录或
临时目录。

## 快速开始

生成三组确定性样本及整帧基线哈希：

```powershell
.\.venv\Scripts\python -m dustrender samples --out artifacts\samples
```

校验场景：

```powershell
.\.venv\Scripts\python -m dustrender validate artifacts\samples\smoke_small.dust
```

分块渲染（配置可直接写在 JSON 中）：

```powershell
.\.venv\Scripts\python -m dustrender render `
  artifacts\samples\smoke_small.dust `
  --config artifacts\configs\seam.json `
  --chunk-size 12x9 --parallelism 3 --budget 100000 `
  --seam --compare-precision --snapshot-each-chunk `
  --out artifacts\render-smoke
```

断点恢复：

```powershell
.\.venv\Scripts\python -m dustrender resume `
  artifacts\samples\smoke_small.dust `
  --snapshot artifacts\render-smoke `
  --config artifacts\configs\seam.json `
  --out artifacts\render-resumed
```

`--snapshot` 也可以直接指向单个快照 JSON 文件。指向目录时会选择最新且通过版本、
场景、配置与哈希链校验的快照。

增量重渲：

```powershell
.\.venv\Scripts\python -m dustrender incremental `
  artifacts\samples\smoke_small.dust `
  artifacts\samples\smoke_small-modified.dust `
  --config artifacts\configs\incremental.json `
  --full-reference --out artifacts\render-incremental
```

图形预览：

```powershell
.\.venv\Scripts\python -m dustrender gui `
  artifacts\samples\smoke_small.dust `
  --config artifacts\configs\seam.json
```

预览窗口展示图像、分块网格、已完成分块着色和预算占用曲线。分块尺寸、并行度、
预算和精度模式可在界面中修改。`Render` 在后台执行，`Cancel` 为协作式取消，旧帧
在整帧发布前持续可见。`OpenGL` 按钮会启动独立的 PyOpenGL 窗口。

本地回环服务：

```powershell
.\.venv\Scripts\python -m dustrender serve --workdir artifacts\serve-work
```

服务只监听 `127.0.0.1`，启动后输出：

```json
{"event":"server_ready","url":"http://127.0.0.1:<port>","root":"..."}
```

可用接口：

- `GET /health`
- `GET /api/samples`（工作目录存在 `samples/manifest.json` 时）
- `POST /api/render`，请求体为 `{"scene": "...", "config": {...}}`

## 主要模块

- `dustrender/scene.py`：DUSTVOL1 解析、相机/介质/嵌套/密度校验与几何构建
- `dustrender/density.py`：块索引、块哈希与三线性密度插值
- `dustrender/geometry.py`：球、轴对齐盒、介质边界和视线分段
- `dustrender/renderer.py`：主步进、阴影可见性、浮点/定点散射累积
- `dustrender/scheduler.py`：稳定任务表、预算规划、并行轨迹、接缝与摘要
- `dustrender/seam.py`：整屏真值比较和接缝像素定位
- `dustrender/incremental.py`：依赖失效、缓存复用与原子版本发布
- `dustrender/snapshot.py`：临时文件加原子重命名、哈希链恢复
- `dustrender/samplegen.py`：本地确定性样本与基线哈希生成
- `dustrender/cli.py`：脚本化入口
- `dustrender/gui.py`、`dustrender/glview.py`：交互预览与 OpenGL 窗口
- `dustrender/server.py`：本地回环 HTTP 服务
- `dustrender/acceptance.py`：完整本地验收套件

场景二进制格式见 `docs/scene-format.md`，设计与确定性约束见 `docs/design.md`。

## 业务错误码

- `E_SCENE_INVALID`：字段缺失、矩阵不可逆、退化几何、嵌套不合法、密度块索引/范围/
  哈希错误；错误包含 `field_path`
- `E_BUSY`：同一调度器已有一个整体渲染运行
- `E_BUDGET_EXCEEDED`：拒绝策略存在超预算分块，或降级到最大步长仍无法满足预算；
  已完成的提交结果保留
- `E_SEAM_UNRESOLVED`：分块结果与整屏单块真值超过容差；详情定位像素和分块对
- `E_RECOVERY_INVALID`：快照版本、场景/密度版本、配置、内容哈希或哈希链无效
- `E_CANCELLED`：协作式取消
- `E_CONFIG_INVALID`：配置字段类型、范围或组合非法

## 确定性摘要与轨迹

每次渲染写入：

- `image.npy`：`float32` RGB，形状 `(height, width, 3)`
- `transmittance.npy`：`float32` 透射率
- `summary.json`：分块表、预算用量、采样数、有效步长、精度比较、接缝结果、哈希
- `result.json`：分块状态、内容哈希和最终错误
- `trace.jsonl`：`render_start`、`budget_plan`、`task_planned`、等待/降级/拒绝、
  worker 启停、接缝、精度和结束事件，全部使用单调 `seq`

图像哈希是 RGB 小端 `float32` 连续字节的 SHA-256；透射率同理。固定输入下连续两次
渲染的缓冲区逐字节一致，轨迹和摘要也不读取系统时间。

## 测试与验收

运行自动化测试：

```powershell
.\.venv\Scripts\python -m pytest -q
```

运行完整本地验收：

```powershell
.\.venv\Scripts\python -m dustrender acceptance --out artifacts\acceptance-run
```

验收覆盖：

- 三种分辨率、1/2/3 个介质、gradient/wispy/uniform 密度分布的样本生成与基线哈希
- 分块渲染、两次渲染逐字节一致、整屏单块接缝比较
- 浮点/定点逐像素误差统计、误差上界和误差图哈希
- 预算降级、预算拒绝、超预算任务等待与已完成结果保留
- 密度块变化后的局部增量重渲、缓存哈希复用、与全帧参考逐字节比较
- 中断快照、原子恢复、不重复渲染已完成块、断链拒绝

本工作区已记录一次完整验收，结果见 `artifacts/acceptance-run/report.json`、各渲染
目录中的 `trace.jsonl` 和 `artifacts/acceptance-run/traces/`。实际运行环境与结果：

- Python `3.11.18`，NumPy `2.4.6`，PyOpenGL `3.1.10`
- 7 项验收检查全部 `passed`
- 接缝最大 RGB/透射率偏差均为 `0.0`
- 定点路径最大绝对偏差 `1.1980533599853516e-05`，声明上界
  `0.019683837890625`，无像素超过上界
- 降级场景有效步长从 `0.08` 增至 `0.433380728`，所有分块采样数均不超过
  `12000`
- 预算拒绝返回 `E_BUDGET_EXCEEDED`，保留已完成分块
- 增量结果与全帧结果图像哈希一致，未受影响分块复用
- 中断快照从已完成索引 `[0, 1]` 恢复，恢复图像与完整渲染哈希一致；人为断链返回
  `E_RECOVERY_INVALID`

`artifacts/acceptance` 保留了题目提供的参考产物；`artifacts/acceptance-run` 是本次
实现实际生成的验收产物。
