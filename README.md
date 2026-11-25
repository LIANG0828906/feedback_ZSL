# Atelier · 开放世界资源采集点分布重建工作台

Atelier 是一个 Windows/Wails 桌面应用，用固定种子的本地样本完成“区域加载 → 分层采样 → 地带密度/稀有度反推 → 采集点重建 → 密度热力面 → 精确失效传播 → 增量重算 → SQLite 检查点恢复”的完整闭环。

项目不依赖容器、外部服务、真实账号或网络素材。Go 模块由 `go mod` 获取，SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。

## 快速运行

Windows 前置条件：

- Go 1.22 或更高版本，`go` 在 `PATH` 中。
- Wails CLI：`go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2`（与 `go.mod` 固定的库版本一致，兼容 Go 1.22）。
- Microsoft Edge WebView2 Runtime。

```powershell
go mod download
go test ./...
go run ./cmd/atelier-cli -verify
wails dev
```

生产构建：

```powershell
wails build
```

桌面应用将检查点数据库写入启动工作目录的 `atelier-checkpoints.db`。前端位于 `frontend/dist`，为纯 HTML/CSS/Canvas，无 npm 构建步骤。

本仓库在 Linux/Go 1.22 环境完成了核心测试、CLI 验收、`GOOS=windows` 交叉编译，以及 `wails build -platform windows/amd64` 的真实桌面入口打包；GUI 运行需要目标 Windows 主机上的 WebView2。

## 可重复验收

```powershell
go run ./cmd/atelier-cli -verify
```

输出：

- `artifacts/verification.json`：逐项检查、逐地带误差、增量/全量结果、检查点恢复结果。
- `artifacts/scenario-seed-2042.json`：生成器产出的完整区域、观测与隐藏真值样例。
- `artifacts/verification-state/checkpoints.db`：断点恢复/作废测试所用 SQLite 文件。

本次实际验收结果（seed 2042）：

| 项目 | 结果 |
| --- | --- |
| 真值采集点 / 重建采集点 | 3319 / 3323 |
| 热力面 | 60×40，2400 像素，cellSize 4 |
| 真值点最大地带偏差 | east-ridge +2；south-marsh +3 |
| 密度最大绝对误差 | 0.000200（声明容差 0.012） |
| 稀有度最大 L1 | 0.001953（声明容差 0.055） |
| 采样格点热力面最大误差 | 0.005000（声明容差 0.018） |
| 密度编辑 | 复用采样；仅失效 1 个地带；1996 像素复用、404 像素重算 |
| 稀有度编辑 | 2400 个热力像素全部复用 |
| 禁采区拖动 | 仅失效 south-marsh；3 个地带、1176 像素复用 |
| 增量 vs 全量 | 点、像素、cache-key 序列逐项相等 |
| 强制终止恢复 | `forced_termination`，复用 2 个已提交地带，恢复结果与全量一致 |
| 主动取消恢复 | `active_cancel` |
| 版本不匹配 | 整个快照作废，剩余地带行数为 0 |

完整逐项输出见 `artifacts/verification.json`，全部检查当前为 `PASS`。

## 样本与真值

`internal/sample` 按种子解析构造：

- 三个独立区域拓扑：边境盆地（4 地带）、赤谷裂原（3 地带）、潮汐边缘（4 地带）。
- 每个区域都有互不重叠地带、3 个禁采区和独立地形栅格。
- 与包围盒相容的 60×40 地形类型栅格（`forest/meadow/ridge/marsh`）。
- 每个地带隐藏的基础密度、稀有度参数、地形密度倍率。
- 隐藏真值点由上述解析参数直接抽样，记录坐标、地形、稀有度与密度参数。
- 观测格点密度由同一解析密度场加微小量化抖动得到；引擎只读取观测值，不读取真值。

区域目录提供 seed 2042（边境盆地）、7183（赤谷裂原）、9911（潮汐边缘）三个独立本地样本。`LoadScenarioJSON` 也可加载生成器导出的样本 JSON，并重新执行结构/几何校验。

## 核心算法

### 1. 校验

- 缺失/非法字段 → `E_SCHEMA`。
- 多边形未闭合、自交、地带重叠、禁采区越界、栅格尺寸不兼容 → `E_GEOM`。
- 几何错误包含地带编号与顶点编号（1-based）。
- 栅格宽度/高度必须等于包围盒按 `cellSize` 向上取整的结果，且 `origin` 与包围盒一致。

### 2. 确定性分层采样

- 以 `SampleGridStep=4` 生成全部候选观测格点，排除禁采区。
- 按地带内地形层分组，以候选数按比例分配每层采样预算，每层至少 1 点。
- 每层候选按 `(gridRow,gridCol)` 排序后等距选取，不使用 map 遍历顺序、线程编号或运行时随机源。
- 所有 cache key 由 SHA-256 内容哈希组成；同一区域、同一配置、同一编辑历史产生相同结果。

### 3. 密度与稀有度反推

对地带 `z` 的地形层 `t`：

```text
ρ̂(z,t) = densityScale(z) × mean(quantized observed density)
A(z,t)  = 地形栅格单元面积之和，排除禁采区
ρ̂(z)    = Σ_t ρ̂(z,t)·A(z,t) / Σ_t A(z,t)
```

稀有度比例由层内观测的稀有度计数聚合；`rarityShift` 只调整估计的 common/rare 比例，并重新归一化。

### 4. 重建与热力面

- 每层重建目标数：`round(ρ̂(z,t) × A(z,t))`。
- 坐标由 SHA-256 派生种子驱动的确定性 SplitMix64 在有效区域拒绝采样。
- 点集按坐标排序并赋予稳定 ID，避免遍历顺序影响。
- 热力面尺寸：`ceil(bounds.width / cellSize) × ceil(bounds.height / cellSize)`。
- 像素中心查询所属地带与地形，取对应层估计密度；禁采区像素为 0。

### 5. 精确失效

依赖链在运行时显式暴露：

```text
exclusion ─┐
terrain ───┼─> sampling ─> estimate ─> reconstruction
params ────┘                          estimate ─> heatmap tile
```

| 编辑 | 采样缓存 | 密度估计 | 重建点 | 热力面 |
| --- | --- | --- | --- | --- |
| 地带 `densityScale` | 复用 | 失效 | 失效 | 失效该地带像素 |
| 地带 `rarityShift` | 复用 | 失效 | 失效 | 全部复用 |
| 禁采区移动 | 仅相交地带失效 | 同左 | 同左 | 仅相交地带像素失效 |
| 地带边界 | 该地带失效 | 失效 | 失效 | 该地带像素失效 |

热力面分块为 8×8 像素。混合分块中只重算失效像素，其他像素逐像素复制旧值。失效清单逐条包含地带编号、分块坐标、复用状态、原因、复用/重算像素数。

**缓存命中序列说明：** 增量与全量比较中的 `cacheHitSequence` 记录每个地带稳定的 `samplingKey/estimateKey/pointsKey` 内容键序列，而不是本次运行中 HIT/MISS 布尔状态。未失效地带的复用情况单独记录在 `invalidation` 清单中。这样可比较“地带级缓存身份序列”，同时保留“全量从空缓存重建”和“增量复用未失效地带”两种语义。

### 6. 增量/全量比对

`CompareWithFull` 从空缓存独立全量重建，逐项比较：

- 采集点坐标、地带、地形、稀有度（首个差异返回 `E_DIFF` + 点序号 + 地带）。
- 热力面浮点值按 IEEE-754 位逐像素比较（首个差异返回像素坐标 + 地带）。
- 地带级 cache-key 序列。

### 7. 偏差传播报告

逐地带记录：

- 采样密度量化误差：`mean(|observed - quantized|)`。
- 层内估计标准误：`sampleStd / sqrt(n)`，并按面积权重合成地带标准误。
- 稀有度取整残差：估计比例分配到重建点数后的取整差。
- 热力面重建误差：采样格点处热力面值与观测密度的平均绝对差。

四项累计上界仅使用观测样本数、观测密度范围、有效面积、量化步长与热力面栅格尺寸：

```text
量化上界      = quantizationStep / 2
标准误上界    = (maxObserved - minObserved) / sqrt(n)
稀有度上界    = roundingResidual + rarityKinds × 0.5 / max(1, points)
热力面上界    = quantizationStep / 2 + (maxObserved - minObserved) / sqrt(n)
```

每项上界的公式与输入在报告中以 `bounds.*.formula` 和 `bounds.*.inputs` 明示，不使用隐藏真值或缺失信息。

### 8. SQLite 检查点

表结构：

- `sessions`：场景内容哈希、算法版本、稳定地带顺序、运行/取消/完成/作废状态。
- `zone_checkpoints`：稳定序号、参数版本、算法版本、payload SHA-256、采样索引/估计/重建点/热力摘要。

每个地带在独立事务中提交，写入受互斥锁串行化，提交顺序始终为稳定地带序。

重启恢复时：

- 场景哈希、会话算法版本、每地带参数版本、每条 payload 内容哈希任一不匹配 → 删除全部地带行并标记会话 `invalid`。
- 会话状态 `running` → 恢复来源 `forced_termination`。
- 显式取消 → `active_cancel`。
- 正常完成 → `completed_checkpoint`。
- 只复用连续前缀；缺失后缀按稳定拓扑序补算。

### 9. 并发资源边界

- 工作线程数固定为 `MaxParallelism`。
- 有界任务队列容量为 `MaxParallelism × PerWorkerBufferSize`。
- 热力分块单元数 `tileSize²` 必须不超过每线程缓冲上限（默认 8×8=64）。
- 队列满时生产者在提交点阻塞，并累计 `bufferBlockEvents` 与 `lastBlockReason`；不会丢弃地带。

## 界面

- 区域俯视、地形栅格、地带着色、禁采区斜线、采样格点、重建点、密度热力面叠加。
- 地带参数滑杆、禁采区位移、增量重算、逐地带重试、全量重建、取消、导出。
- 失效传播图、热力分块复用/重算矩阵、四项累计上界曲线。
- 逐地带偏差报告、地形层估计、运行指标、检查点与恢复来源。
- 出错时显示 `E_SCHEMA`、`E_GEOM`、`E_DIFF`、`E_CANCEL`、`E_IO`，`E_DIFF` 同时显示首个不一致点/像素与地带。

## 目录

```text
cmd/atelier-cli/       跨平台验收与样本导出
frontend/dist/         Wails 静态前端
internal/checkpoint/   SQLite 会话与分地带检查点
internal/engine/       采样、估计、重建、热力面、失效、比对、报告、线程池
internal/geometry/     多边形与相交判定
internal/model/        JSON 数据契约
internal/sample/       固定种子生成器与隐藏真值
internal/scenarioio/   样本 JSON 读写
internal/validate/     结构/几何校验
internal/workspace/    Wails 服务层
docs/ARCHITECTURE.md   失效矩阵、缓存键、检查点细节
docs/ACCEPTANCE.md     实际验收记录
```
