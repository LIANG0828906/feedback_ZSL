# VolumeLab

面向医学影像阅片场景的本地确定性容积数据工作站。项目包含：

- 确定性样本生成器：固定种子生成标量体数据与解析几何真值清单。
- 三平面重采样：轴位、冠状位、矢状位共享世界坐标定位点。
- 窗宽窗位、最近邻/三线性插值、层厚重叠加权。
- 双坐标测距标注：世界坐标与体素坐标同时输出。
- 解析真值精度验收与可推导偏差上界。
- 原子、串行、版本校验的会话快照及跨会话恢复。
- 有界线程池、每任务像素缓冲上限、串行/并行确定性一致。
- Wails 桌面壳与无需 npm 的静态前端；另提供本地 HTTP 入口便于无 GUI 环境验收。

所有样本、会话和验证产物都在本机生成，不访问外部服务或真实医学数据。

## 快速开始

要求 Go 1.22+。浏览器演示不需要 npm；Wails 桌面壳复用同一静态前端，不需要前端构建。

```bash
# 1. 生成固定种子样本
go run ./cmd/samplegen -out samples -seed 42

# 2. 一键验收并写出机器可读报告
go run ./cmd/verify -out artifacts/verification.json

# 3. 启动本地阅片工作站
go run ./cmd/volumelab \
  -addr 127.0.0.1:8090 \
  -manifest samples/manifest.json \
  -session-dir samples/sessions
```

浏览器打开 `http://127.0.0.1:8090`。三平面画布可单击定位；切换“测距”后在同平面单击两次完成标注。右侧“解析真值验收”可载入球/管边界标注并执行精度校验。

也可使用 `make generate`、`make test`、`make race`、`make verify`、`make run`。

Wails 桌面壳（静态前端已嵌入，`-s` 跳过 npm 构建）：

```bash
cd desktop
wails build -s
# Ubuntu 24.04+/仅安装 WebKitGTK 4.1 时：
# wails build -s -tags webkit2_41
./build/bin/volumelab-desktop
```

Linux 构建 Wails 需要系统包 `pkg-config`、`libgtk-3-dev`、`libwebkit2gtk-4.1-dev`。本交付环境缺少这些开发包且当前账号无 sudo 权限，详见“实际验证记录”；桌面模块的普通离线 `go build` 已通过，浏览器入口与全部核心测试已实跑。

## 目录

```text
cmd/samplegen       确定性体数据/真值清单生成器
cmd/volumelab       本地 HTTP + 嵌入式 Web UI
cmd/verify          一键验收
pkg/volume          文件格式、清单、轴序和定标校验
pkg/render          三平面反采样、窗宽窗位、层厚加权
pkg/measure         双坐标测距、真值偏差、上界
pkg/session         状态机、原子快照、恢复/取消
pkg/pool            有界并发和像素缓冲限制
pkg/service         统一 API，供 HTTP 和 Wails 共享
pkg/httpserver      本地 JSON API 与静态文件服务
pkg/webui           无 npm 依赖的三平面交互界面
desktop             Wails v2 桌面入口（独立 Go module）
artifacts           环境记录与验收报告
samples             仓库内固定种子样本
```

## 样本格式

`samples/manifest.json` 声明每个体数据的：

- `axisOrder`：存储轴顺序，字符为 `fastest-to-slowest`，支持 `XYZ`、`ZYX` 等排列。
- `dimensions`：物理 `[X,Y,Z]` 尺寸。
- `spacing`：物理轴间距，单位 mm，必须有限且为正。
- `slope` / `intercept`：原始标量到定标灰度的映射。
- `layerCount`：Z 轴层数，必须等于 `dimensions[2]`。
- `grayRange`：窗位允许的声明灰度范围。
- `sha256`：体数据文件内容哈希。
- `structures`：球心/半径、管轴端点/半径、矩形块包围盒，以及所有结构的解析包围盒。

体数据文件 `*.vlm` 为固定 96 字节小端头 + `int16` 标量载荷：

```text
0..7    magic "VLMVOL01"
8..11   version
12..14  storage axis order
16..27  storage dimensions
28..39  physical dimensions
40..63  spacing [X,Y,Z]
64..71  slope
72..79  intercept
80..83  layerCount
84..91  payload byte count
92..95  reserved
```

轴序不是简单的展示字段：存储偏移由 `axisOrder` 和实际维度共同计算。例如 `ZYX` 下 Z 轴最快变化、X 轴最慢变化。清单声明与文件头/载荷结构逐项比对。

样本内的球、管、块具有确定性纹理，但边界位置由解析几何决定。解析真值只用于验收，不参与图像渲染或偏差上界推导。

## 重采样与显示

每个平面使用原生体素网格输出：

- Axial：水平 X，垂直 Y，法向 Z。
- Coronal：水平 X，垂直 Z，法向 Y。
- Sagittal：水平 Y，垂直 Z，法向 X。

像素世界坐标为 `index * spacing`。法向使用共享世界定位点；平面内位置使用当前平面网格。插值阶数 `0` 为最近邻，`1` 为三线性。输出先按 `raw*slope+intercept` 得到定标灰度，再映射：

```text
display = round(clamp((value - (level - width/2)) / width * 255, 0, 255))
```

层厚等效按层与厚层区间的物理重叠长度加权：

```text
w_layer = overlap([p-t/2, p+t/2], [k*s-s/2, k*s+s/2])
weight  = w_layer / sum(w_layer)
```

这也覆盖层位落在两层之间的情况。每个平面允许层厚区间为 `[0, (layers-1)*normalSpacing]`。

## 双坐标测量

标注保存：

- 端点 A/B 的世界坐标 `(mm)` 与体素坐标。
- `worldLengthMM = sqrt(sum((dv_i*s_i)^2))`。
- `voxelLength = sqrt(sum(dv_i^2))`。

恢复快照时会重新检查坐标互换算和两种长度；不一致的旧数据会使整个快照作废。

解析精度校验按结构类型派生真值长度：

- 球：直径 `2r`。
- 管：轴线端点距离。
- 矩形块：包围盒对角线。

偏差上界不读取解析真值：

```text
bound = 2 * sqrt(sum_axis(
  (spacing_axis * (0.5 + 0.5*interpolationOrder)
   * (1 + 0.25*maxLayerIndex_axis/(layers_axis-1)))^2
))
```

其中只用已观测的体素间距、插值阶数、端点所在层索引和轴层数。测试专门验证修改解析半径不会改变上界。实测偏差超过声明容差或该上界时返回 `E_PREC`。

## 快照与恢复

快照路径默认为 `<清单目录>/sessions/<volumeId>.session.json`。快照 payload 包含：

- 体数据 ID、内容 SHA-256、清单 SHA-256。
- 窗宽窗位、共享定位点、三平面层厚、插值阶数。
- 全部标注、A/B 双坐标读数、两种长度。
- 操作序号、下一标注序号、三平面像素 SHA-256。
- 快照格式版本与重采样算法版本。

提交过程：

1. 会话互斥锁保证操作顺序。
2. 所有修改先落在不可见草稿上。
3. 草稿完整重采样后，计算 payload 与校验和。
4. 写入同目录临时文件、`fsync`、原子 `rename`、同步目录。
5. 原子发布成功后才替换内存状态。

恢复时先校验 envelope、checksum、版本、内容哈希、状态字段，再重采样并比对三面像素哈希。任一项不匹配都会删除旧快照并从默认状态全量重建，不会部分混用旧数据。

取消只丢弃当前草稿，不触碰已提交快照。进程被强制终止时，恢复点仅为最后一次成功 `rename` 的快照；不承诺补全被杀进程未提交的会话报告。测试通过真实子进程 `SIGKILL` 验证这一点。

## 并发与资源边界

`pkg/pool.Executor` 固定最大 worker 数，并把每任务像素数限制在 `maxPixelsPerWorker`。平面任务被拆分为不超过该上限的连续像素段；槽位或队列满时调用方阻塞，不会静默丢弃任务。每个输出像素由唯一任务写入，无跨任务归约，因此：

- 线程数与任务完成顺序不影响像素。
- 单线程与并行路径产生相同像素哈希。
- 会话提交仍由互斥锁串行化并保留 `sequence`。

默认最大并发度为 3，每任务 16384 像素。CLI 可通过 `-max-parallel` 与 `-max-buffer-pixels` 调整。

## 错误码

| 错误码 | 触发条件 | 关键字段 |
|---|---|---|
| `E_AXIS` | 轴序不合法、轴序与存储维度冲突、物理维度/层数冲突 | `axis` |
| `E_CAL` | 间距非正/缺失、斜率/截距/灰度范围缺失或冲突 | `field` |
| `E_DECODE` | 文件头非法、载荷截断或长度不符、内容哈希不符 | `actual` / `allowed` |
| `E_WIN` | 窗宽非正、窗位越界 | `field`、`actual`、`allowed` |
| `E_THICK` | 层厚为负或超过该平面范围 | `actual`、`allowed{min,max,layers,spacing}` |
| `E_RANGE` | 标注端点越界 | `endpoint`、`axis` |
| `E_PREC` | 实测偏差超过容差/上界 | 同时返回 `PrecisionReport` |
| `E_CANCEL` | 当前操作被取消 | 已提交状态不变 |
| `E_VERSION` | 快照版本/内容哈希/算法或像素哈希不匹配 | 快照整体作废 |

## API

HTTP `POST /api/invoke`：

```json
{
  "action": "set_window",
  "payload": { "width": 800, "level": 200 }
}
```

主要 action：

```text
status, generate, open, state, pause, close, cancel
set_window, set_crosshair, set_plane_position,
set_thickness, set_interpolation
add_annotation, auto_annotation, delete_annotation, precision
```

Wails 通过运行时注入的 `window.go.<package>.App.Invoke(action, payloadJSON)` 调用完全相同的服务层；前端会自动发现绑定包，当前为 `gui.App`。

## 测试

```bash
go test ./...
go vet ./...
go run ./cmd/verify -out artifacts/verification.json
```

测试覆盖：

- 样本同种子逐字节确定。
- 轴序/维度、定标、截断错误码与字段。
- 三平面串并行逐像素一致、重复渲染一致。
- 层厚权重归一、窗宽窗位和层厚越界。
- 世界/体素双读数互校验及端点越界轴。
- 球/管解析真值、偏差上界不依赖真值。
- 快照恢复数值/像素一致、版本和内容哈希失效全量重建。
- 取消不污染快照、恢复后继续操作与未中断路径一致。
- 真实 `SIGKILL` 后仅恢复已提交快照。
- HTTP/Wails 共用服务层的端到端工作流。

## 实际验证记录

环境与本机实跑结果记录在：

- `artifacts/environment.txt`
- `artifacts/verification.json`
- `artifacts/http_restart_check.json`

本次 `go run ./cmd/verify` 的 9 项检查全部通过，包括三面像素哈希、双坐标长度、偏差上界、快照恢复、取消语义以及串并行提交 checksum 一致。另以真实本地 CLI 进程完成“写入快照 → `SIGKILL` → 重启 → 恢复”检查，sequence 4 的状态、16 mm 双坐标标注和三面像素哈希全部一致。
