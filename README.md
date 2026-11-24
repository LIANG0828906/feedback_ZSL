# VectorLab：确定性矢量路径合成原型

这是一个不依赖 Docker、外部数据库服务或运行时网络的 Go/Wails 原型。核心引擎负责多版本场景加载、整型时间/坐标换算、贝塞尔插值与扁平化、扫描线覆盖、抗锯齿合成、SQLite 快照恢复，以及“实时预览 / 断点恢复 / 全量重算”三条路径的逐像素一致性核验。

## 快速开始

前置条件：

- Windows 10/11
- Go 1.22+
- Wails CLI：`go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- `wails doctor` 显示 WebView2/构建依赖可用

工作区命令：

```powershell
go mod download
go test ./...

go run ./cmd/scenegen -out artifacts/sample
go run ./cmd/vectorlab -generate=false

wails build -tags wails
.\build\bin\vectorlab.exe
```

Wails 入口使用 `wails` 构建标签，因此普通 `go test ./...` 不会被 Linux/macOS 的 WebView 系统库阻塞。GUI 启动后从工作区读取 `artifacts/sample`，并在 `artifacts/ui` 写 SQLite。若不构建 GUI，CLI 已覆盖全部可自动验收的引擎路径。

## 验证结果

本工作区实际运行结果保存在：

- `artifacts/sample/`：固定种子 `0x5eed2026` 的三版本场景、采样时刻与真值。
- `artifacts/verify/verification.json`：加载、往返、真值、质量、恢复和一致性总报告。
- `artifacts/verify/runs/{preview,full,resume-interrupted,resume}/`：三路径 PNG 与提交序列；中断目录保留 2 帧前缀。
- `artifacts/verify/snapshots.db`：全量路径 SQLite 快照；`resume.db` 保存中断/续算证据。

本轮结果：

| 检查项 | 结果 |
|---|---|
| v1/v2/v3 加载与语义互操作 | 3/3 通过 |
| 跨版本往返 | 9/9 通过 |
| 真值曲线点、面积、包围盒、覆盖比例 | 通过 |
| 精细模式（4×4 AA，Q10 容差 64） | 自动选中 |
| 近似模式（2×2 AA，容差 256，8 bit 插值精度） | 5/5 帧误差通过 |
| 近似/精细覆盖面积最大相对误差 | `0.000279` |
| 近似/精细包围盒 IoU | `1.0` |
| 断点恢复 | 停止于第 2 个提交，重启后补完 5/5 |
| 三路径最终位图与提交序列 | 完全一致，`E_DIFF` 未触发 |
| `E_LOSSY` 降级报告 | `replace -> source-over`、`relative-delta -> absolute-q10` |

测试命令：

```bash
go test ./... -count=1
CGO_ENABLED=0 go test -tags wails ./... -count=1
```

第二个命令验证 Wails 后端绑定与诊断输出；当前 Linux 环境缺少 WebKit，因此没有实际弹出窗口。

## 目录结构

```text
engine/                 核心引擎与测试
  loader.go             三版本解析、统一模型、E_SELF/E_SCHEMA/E_COMP 校验
  units.go              Q10/微秒整型换算与 E_RT
  interp.go             线性/阶跃/平滑整数插值
  flatten.go            曲率/容差联动递归细分
  raster.go             扫描线 AA 覆盖、描边连接、合成与内容哈希
  quality.go            分级精度与 E_SCALE
  export.go             跨版本导出、E_LOSSY 与 E_EQ
  state.go              三阶段四状态机
  store.go              SQLite 版本化快照
  runner.go             三路径、工作池、暂停/取消、稳定提交
  compare.go            三路径逐像素比对与真值核验
  sample.go             固定种子样本/真值生成器
cmd/scenegen/           样本生成入口
cmd/vectorlab/          CLI 验证入口
frontend/dist/          无外部依赖的 Wails 静态诊断页
app.go                  Wails 后端绑定（wails 构建标签）
main.go                 Wails 入口（wails 构建标签）
```

## 内部模型与 schema

内部坐标固定为 Q10：`1024` 单位 = 1 逻辑像素。内部时间固定为微秒 tick：`1000` tick = 1 ms。样本时钟为 100 fps，即 `10000` tick = 1 帧，因此样本中的帧/毫秒换算精确闭合。坐标与时间是整型，关键帧插值和相对增量展开也使用整型运算；浮点只用于最终像素覆盖归一化和 PNG/诊断展示，不参与 schema 往返累积。

| 版本 | 时钟 | 控制点表达 | 关键字段差异 |
|---|---|---|---|
| v1 `1.0` | `ms` | 绝对 Q10 | `schemaVersion`、`paths[].keyframes[]`、`fillColor[]`、`strokeWidthQ10`、可选 `blend` |
| v2 `2.0.0` | `frame` | 绝对 Q10 | `schema_version`、`keys[]`、`curves[]`、`composite`、`aaGrid`、`renderScale` |
| v3 `3` | `ms` | 相对 keyframe `base` 的 `dx/dy` | `flags[]`、`fill{}`、`stroke{widthQ10,join}`、必填 `blend`、`interp{}` |

v1/v2 不声明支持 `replace` 合成模式；导出到这两个版本时生成 `E_LOSSY` 并降级为 `source-over`。相对坐标导出到 v1/v2 时同样生成 `E_LOSSY`，处置方式为展开成绝对 Q10。

## 错误码

| 错误码 | 触发点 |
|---|---|
| `E_SELF` | 段首尾不连续、轮廓不闭合、关键帧时间非严格递增、拓扑变化 |
| `E_VER` | 未知版本号 |
| `E_SCHEMA` | 必填字段缺失或 JSON 类型错误，带字段路径 |
| `E_RT` | 帧/毫秒或绝对/相对换算往返不闭合，带首个字段与两侧值 |
| `E_PARA` | 插值方式、精度、容差、缩放、子采样越界 |
| `E_COMP` | 合成模式非法、颜色/位图通道数不符，带路径 ID |
| `E_SCALE` | 近似/精细覆盖或包围盒指标超差，带帧号和指标 |
| `E_LOSSY` | 非致命降级警告，记录字段路径、原值与处置方式 |
| `E_EQ` | 导出/升级/降级后首个语义不等价字段 |
| `E_STATE` | 非法状态迁移，原状态保持不变 |
| `E_DIFF` | 三路径或真值比对不一致，带帧、像素、字段 |

## 几何、覆盖与精度

- 曲线使用 de Casteljau 整型细分；线段数由控制点到弦的最大距离与容差决定。容差收紧只会增加或保持递归分裂，因此同一路径同一帧的折线段数单调不减。
- 填充覆盖采用逐子扫描线边求交，再合并区间统计子样本；支持 `nonzero` 与 `evenodd`。描边先由线段梯形和 `round/bevel/miter` 连接形状组成并集，再参与同一套子样本覆盖。
- 位图尺寸由当帧扩展包围盒和 Q10 缩放推导：`ceil(width * scale / 1024²)`。AA 网格决定每像素内部子样本数，不改变逻辑包围盒。
- 合成支持 `source-over`、`multiply`、`screen`、`replace`，路径透明度与颜色 alpha 分别进入覆盖。
- 质量选择估算 `路径数 × 关键帧数 × 采样时刻数 × 像素缓冲`。预算足够固定走精细模式；不足时按预估规模选择 2×/4× 降采样、减小 AA 网格、放宽容差并降低插值精度。每个决定都记录模式、倍率和原因。近似/精细的一致性门槛为覆盖面积相对误差 `≤ 0.03`、包围盒 IoU `≥ 0.95`。

## 快照与三路径

SQLite 表 `frames` 每个采样时刻一行，在同一事务内写入：

- 所有路径折线与覆盖分量；
- 内容 SHA-256；
- 插值精度与容差；
- 参数版本、合成模式版本、算法版本与快照规范版本；
- 稳定提交序号。

写入由互斥锁串行化。恢复时按 `commit_seq` 校验连续性、时间顺序、版本和重算哈希；任一不匹配会删除该场景全部快照行并从头重算。并行工作池只负责计算，提交器按采样序号等待前序完成，因此提交顺序不随 goroutine 调度变化。

三种路径：

1. `preview`：串行渲染并即时返回帧结果。
2. `full`：清空快照后并行全量重算。
3. `resume`：校验已有前缀，从首个缺失采样时刻继续。

`CompareRuns` 比较提交序列、折线段数、位图尺寸、内容哈希和 RGBA；发现差异时返回 `E_DIFF`，并定位首个帧号、像素坐标与字段名。

## Wails 诊断界面

界面包含：

- 当帧位图，以及基于同帧解析折线的轮廓/笔触叠加；
- 相对精细基线的差异热力图；
- 每路径、每采样时刻的 `flatten / coverage / composite` 状态矩阵；
- 质量模式、降采样倍率、判定依据和逐路径覆盖比例；
- 容差与插值精度调节后的单帧增量重绘；
- 三路径运行、暂停、继续、取消和逐时刻重试。

后端资源边界为最多 8 个工作线程；当估算像素缓冲超过每线程上限时，该时刻进入独占阻塞队列并记录原因，随后仍以稳定序号提交。

## 约束说明

- 样本、PNG、SQLite 与报告全部由本地程序生成；运行时无 HTTP/WebSocket/外部服务请求。
- 样本生成器只使用固定种子和内置几何参数，不使用外部素材。
- 当前执行环境为 Linux + Go 1.22.12 + Wails CLI 2.10.2，缺少 `libwebkit2gtk`，因此实际 Wails 窗口构建需在目标 Windows 环境执行；核心引擎、CLI 和 Wails 后端已在本机完成测试/编译检查。
