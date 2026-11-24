# OcclusionLab

遮挡剔除、视锥投影与场景流式加载的可视化诊断工作台。项目使用固定种子生成本地场景工程，在没有外部数据或网络服务的情况下验证：

- 量化整型/定点精度下的相机变换、包围体投影与视锥测试。
- 遮挡体栅格覆盖、屏幕空间深度比较和 `visible / occluded / uncertain` 三态判定。
- 由定点步长、栅格分辨率、包围体尺寸和相机距离推导的误差传播上界。
- 流式窗口状态机、SQLite 窗口级检查点、断点恢复和全量重放。
- 实时逐窗口、断点恢复、一次性全量重放三条路径的逐窗口一致性。
- Wails 桌面诊断界面，以及不依赖 WebKit 的本地 HTTP 诊断模式。

## 快速运行

### Windows + Wails

前置条件：Go 1.22+、Wails CLI 2.x、WebView2 Runtime、可用的 C 工具链。

```powershell
go version
wails version
.\scripts\check-env.ps1
go mod tidy

# 开发模式
.\scripts\wails-dev.ps1

# 不使用 Wails CLI 的开发启动
go run -tags wails .

# Windows amd64 构建
.\scripts\wails-build.ps1
```

等价命令：

```powershell
$env:CGO_ENABLED = "1"
wails dev -tags wails -skipbindings -s
wails build -tags wails -skipbindings -s -platform windows/amd64 -clean
```

本项目没有 npm 构建步骤。Wails 直接嵌入 `frontend/dist`，通过 `-s` 跳过前端构建，通过 `-skipbindings` 跳过生成 `wailsjs`；运行时仍使用 Wails 的动态 `Bind` 绑定。

### Linux / WSL 本地诊断模式

本地诊断模式使用同一套服务和前端，不链接 GTK/WebKit：

```bash
CGO_ENABLED=0 go run . -addr 127.0.0.1:34115
# 浏览器打开 http://127.0.0.1:34115
```

`OCCLUSIONLAB_DATA` 可指定数据目录；默认保存到 `./data`。桌面版默认使用用户配置目录下的 `OcclusionLab`。

### 生成场景与验收

```bash
CGO_ENABLED=0 go run ./cmd/scene-gen \
  -seed 1042 -count 3 -out data/projects -tolerance-px 2

CGO_ENABLED=0 go test ./...

CGO_ENABLED=0 go run ./cmd/verify \
  -seed 1042 -raster 128 -workers 2 \
  -out artifacts/validation-report.json
```

`cmd/verify` 会执行投影精度、真值兼容性、误差界覆盖、容差单调性和三路径一致性检查；失败时以非零状态退出。`scripts/verify.sh` 还会生成并验收 1042/1043/1044 三套场景。仓库随附样本位于 `artifacts/projects/seed-*`。

## 项目文件格式

生成器按固定种子写出四个确定性文件：

```text
project.json   schema/version、种子、定点精度、容差
world.json     世界包围体、分块、遮挡体、相机轨迹
truth.json     每窗真值、参考遮挡体、已知屏幕投影
manifest.json  三个文件 SHA-256、整体内容哈希
```

`world.json` 中所有几何量都是整型。`world_frac_bits`、`basis_frac_bits`、`screen_frac_bits`、`param_frac_bits` 是全局精度契约。真值由生成器用 `math/big.Rat` 对已量化整数做精确有理运算产生，不复用运行期定点评估器。

## 数值与误差模型

### 定点链路

1. 世界坐标、相机位置、包围体顶点均为 `Q(world_frac_bits)`。
2. 相机基与朝向为 `Q(basis_frac_bits)`；点积后按声明位宽舍入。
3. 屏幕坐标、焦距与中心为 `Q(screen_frac_bits)`。
4. 视锥切线与近远平面参数为 `Q(param_frac_bits)`。
5. 每次点积和屏幕除法都保存整数商、余数、除数、位宽与换算到屏幕量子的残差。

视锥平面在相机空间构造：

```text
near:   z - near >= 0
far:    far - z >= 0
left:   x + tan_half_x * z >= 0
right:  tan_half_x * z - x >= 0
bottom: y + tan_half_y * z >= 0
top:    tan_half_y * z - y >= 0
```

包围体 8 个顶点全部投影并参与屏幕范围、平面余量和残差累计。`Frustum.Status` 是包围体级状态，`Frustum.AnchorStatus` 是用于三态判定的锚点状态。

### 投影误差上界

每个分块的投影误差上界由以下已观测值推导，不使用真值反推：

```text
world_step      = 2^-world_frac_bits
basis_step      = 2^-basis_frac_bits
rel_abs         = |anchor - camera|_1 + bbox_span / 2
dot_error_world = rel_abs * basis_step/2
                + basis_abs * world_step/2
                + world_step*basis_step
                + dot_ladder_residual
projection      = focal * (camera_x_error + ratio*camera_z_error)/(depth-camera_z_error)
                + ratio*focal_error + screen_center_quantum + screen_div_residual
```

报告中的 `dot_product_terms=3`、`projection_ladder_ops=4`、`plane_ladder_ops=4`、`raster_cell_allowance=1` 是显式算法常量。累计可见性上界为：

```text
upper = projection_error
      + one_raster_cell
      + focal * depth_compare_error / camera_distance
      + focal * frustum_plane_error / camera_distance
      + one_screen_quantum
```

### 三态判定

- 锚点明确位于视锥外：`occluded`，参考原因 `frustum`。
- 锚点靠近视锥/屏幕边界，误差带跨越边界：`uncertain`。
- 遮挡栅格查询覆盖 `投影误差 + 一个栅格单元` 的范围：
  - 所有候选单元都由更近遮挡体覆盖：`occluded`，并给出参考遮挡体。
  - 所有候选单元均未遮挡且深度明确：`visible`。
  - 候选单元混合、边界覆盖或深度比较落入误差带：`uncertain`，同时输出 `visibility_lower_q16=0`、`visibility_upper_q16=65536` 和深度上下界。

因此栅格越粗，不确定集合越保守；不会把误差带内的分块强行判成可见或遮挡。

## 窗口状态与检查点

合法窗口迁移：

```text
pending -> culling -> loading -> committed
   |          |          |
   +----------+----------+-> failed -> pending (retry)
```

`committed` 是终态。非法迁移返回 `E_STATE`，原状态不变。已提交窗口的可见集合、被遮挡/不确定集合、驻留集合、加载/卸载序列和误差界均冻结。

每个窗口提交时，在单写者互斥锁和 SQLite 事务中串行写入：

- 窗口顺序号、完整冻结结果、参数版本与参数哈希。
- 可见集合摘要、逐窗结果哈希、前序链式哈希。
- 场景哈希、schema/store 版本。

写入顺序必须连续。加载时只要发现版本、场景哈希、内容哈希或链式哈希不匹配，就删除全部窗口检查点并整体作废；应用随即全量重放，不允许混用旧窗口结果。重启后从最近连续已提交窗口恢复。

## 资源边界

- 工作线程池最多 `max_workers` 个并发剔除任务。
- 每线程栅格缓冲上限为 `raster_buffer_cells`；`raster_resolution^2` 超出时返回 `E_PARA` 并定位 `raster_resolution`。
- 工作线程已被占满时，任务阻塞排队并记录 `worker_slot_wait` 事件，不静默丢块。
- 可见与不确定分块作为驻留目标；驻留槽位或加载成本不足时窗口进入 `failed`，等待重试，不截断结果。

## 界面

工作台提供：

- 世界俯视分块着色、视锥、遮挡体和驻留描边。
- 屏幕空间投影覆盖与参考遮挡体。
- 投影、覆盖、累计上界随窗口变化曲线。
- 三类可见性图例与误差解释卡片。
- 窗口状态矩阵、逐窗分块证据表、加载槽位与加载/卸载序列。
- 实时/断点恢复/全量重放的路径卡片和验收检查项。
- 参数重算、相机平移、暂停、单步、重试、清空重放和工程导出。
- 失效窗口及原因、资源排队事件和运行日志。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_SCHEMA` | 字段缺失、包围体非法、相机时间非单调、遮挡体引用越界等 |
| `E_PARA` | 栅格分辨率、每线程缓冲、量化位宽或容差参数越界 |
| `E_STATE` | 非法窗口迁移、修改已提交窗口、对非失败窗口重试 |
| `E_VERSION` | schema/store/场景版本不匹配 |
| `E_CHECKPOINT` | 检查点写入或顺序错误 |
| `E_IO` | 项目、SQLite 或文件系统 I/O 错误 |
| `E_INTERNAL` | 单窗口内部失败，可重试 |

`E_SCHEMA`/`E_PARA` 的 JSON 与 Go 错误都带 `window`、`chunk`、`field`、`message`、`detail` 定位信息。

## 实际验证结果

本机环境：

- Go `go1.22.12 linux/amd64`
- Wails CLI `v2.10.2`
- Windows amd64 Wails 构建成功：`build/bin/OcclusionLab.exe`，PE32+ x86-64 GUI 可执行文件，约 13 MB
- 本地 HTTP 诊断工作台实际启动并通过 `curl` 逐窗推进、验收和重启恢复
- 主机缺少 GTK/WebKit 开发库，因此未在本机启动 Linux Wails 窗口；Windows 产物已完成交叉构建

验收报告 `artifacts/validation-report.json`：

```text
seed=1042 project=35ad9e31c181 windows=6 chunks=15 raster=128 workers=2
projection: 90/90 within tolerance, max_error=0.003906 px,
            max_bound=0.558594 px, bound_violations=0
visibility: truth_visible=26 truth_hidden=64
            observed visible/uncertain/occluded = 9/17/64
            conflicts=0 conservative_match=90
error: residual=0.070312 px projection=0.558594 px
       plane=0.167969 world coverage=10.558594 px depth=0.003906 world
       cumulative=11.644531 px
paths_equal=true checkpoint_verified=true differences=0
all 8 acceptance checks PASS
seed 1043/1044: all 8 checks PASS, paths equal, conflicts 0
```

自动化测试：

```text
go test ./...                           PASS
blocked projection/visibility checks    PASS
projection tolerance monotonicity       PASS
three-path equality                     PASS
restart from 2 committed windows        PASS
snapshot whole-invalidation on tamper   PASS
window state/illegal transition         PASS
Wails Windows cross-compile             PASS
jsdom DOM/Canvas render smoke test      PASS
```

相机位置调整仅对未提交窗口生效；已提交窗口返回 `E_STATE`。未提交窗口的相机覆盖是运行期诊断输入，不会改写生成器的原始 `world.json`，因此重启后只恢复已提交的冻结结果。

## 目录

```text
cmd/scene-gen          确定性场景工程生成器
cmd/verify             验收报告 CLI
internal/generator     布局、相机、精确真值
internal/fixed         定点舍入与残差记录
internal/geometry      投影、视锥、遮挡栅格、误差传播
internal/schema        项目加载、字段级校验
internal/streaming     状态机、工作池、窗口结果
internal/store         pure-Go SQLite 检查点
internal/verify        三路径与验收
internal/workbench     Wails/HTTP 共用服务
internal/httpui        无 WebKit 的本地 HTTP 适配器
frontend/dist          原生 HTML/CSS/Canvas 界面
scripts                Windows/WSL 构建与验收脚本
```

更细的错误传播和状态语义见 `docs/TECHNICAL.md`。
