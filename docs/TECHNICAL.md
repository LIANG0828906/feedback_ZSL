# 技术说明

## 1. 目标与边界

本工程验证四个独立风险：

1. 固定种子地形数据能否在严格校验下稳定加载。
2. 同一块在不同 worker、不同完成顺序和预览/精确模式下能否逐位一致。
3. 接缝连续性处理是否可解释，并能区分真实几何折角与烘焙伪突变。
4. 增量重烘焙、断点恢复和全量重建是否会收敛到同一像素快照。

所有数据、检查点和导出文件均为本地文件。核心验证不需要 Node、GPU、Docker 或网络服务。

## 2. 数据契约

工程 JSON 顶层字段：

```text
schemaVersion, seed, revision, world,
parameters, layers, blocks, seams, truth
```

块字段：

```text
id, name, width, height, originX, originY, heights, weights
```

约束：

- `len(heights) == width * height`
- `len(weights) == width * height`
- 每行权重长度等于材质层数量
- 每个权重是 `[0,1000]` 的整数
- 每行权重和为正；混合时按该行实际总和归一化（样本行和为 `1000`）
- 共享边两侧采样列/行长度相等
- 接缝引用块存在，且几何原点与共享边重合
- 每块存在解析真值，真值像素在块内
- 每条接缝真值包含解析相邻法线夹角与容差

共享边块采用“共享采样列/行”布局：例如垂直接缝要求
`A.originX + A.width - 1 == B.originX`，且两侧 `height` 相同。

## 3. 法线计算

对高度场 `h(x,y)`：

```text
base = normalize(-dh/dx * scale, -dh/dy * scale, 1)
```

支持的离散梯度：

- `central`: `(h[x+1]-h[x-1])/2`，边界 clamp
- `forward`: `h[x+1]-h[x]`，边界 clamp
- `sobel`: 3×3 Sobel，边界 clamp

材质层具有切线空间偏置 `(biasX,biasY,biasZ)` 和强度 `strength`：

```text
layerNormal = normalize(base + bias * strength)
```

`weighted` 模式：

```text
n = normalize(sum(weight_i / rowSum * layerNormal_i))
```

`dominant` 模式采用最大权重层，平权时选择索引更小的层。

坡度角：

```text
slope = acos(clamp(n.z,-1,1)) * 180 / pi
```

法线颜色码：

```text
rgb = round((n * 0.5 + 0.5) * 255)
```

包围盒统计最终法线三分量的最小/最大值。

## 4. 确定性

像素循环按 `y` 外层、`x` 内层、材质层索引升序执行。每个像素只依赖输入数组和参数，不读取：

- goroutine 编号或线程 ID
- 调度完成顺序
- 墙钟时间
- 并发 map 遍历结果

线程池只负责调用纯函数 `BakeBlock`。测试使用 32 路并发、交替预览/精确模式和不同瓦片缓冲，对法线、坡度、颜色和包围盒做严格位模式比较。

## 5. 接缝连续处理

接缝按 ID 升序处理。每条接缝每轮对超过阈值的边界像素对执行：

```text
nA' = normalize((1-alpha) * nA + alpha * nB)
nB' = normalize((1-alpha) * nB + alpha * nA)
```

`alpha` 来自 `parameters.seamRelaxation`，范围 `(0,0.5]`。所有接缝按稳定顺序执行，迭代次数不超过每条接缝的 `maxIterations`。结束后逐像素复检，仍有超标则返回 `E_SEAM`。

### 归因规则

对每个原始违反像素对：

1. 若高程跳变非零，或两侧较大坡度超过该接缝 `geometrySlopeThresholdDeg`：
   - `geometric_fold`
   - 依据记录 `heightJump`、`slope`、`geometryThreshold`
2. 否则若两侧量化权重平均 L1 差超过 `0.06`：
   - `material_mix`
   - 依据记录 `heightJump`、`slope`、`weightDiff`
3. 否则：
   - `difference_operator`
   - 差异来自两侧独立边界 clamp 后差分采样不同

输出包含两侧局部坐标、法线、夹角、类别和 evidence 字符串。

## 6. 依赖图与失效传播

块依赖图：

- 共享边生成双向邻接边。
- 材质层节点记录所有权重非零的使用块。
- 每个块记录相邻块、使用层、关联接缝。

失效来源：

| 编辑 | 初始失效集合 |
|---|---|
| 块高程 | 该块，再沿接缝双向闭包 |
| 材质层参数 | 所有使用该层的块，再沿接缝闭包 |
| 差分算子/缩放 | 全部块 |

增量任务通过 Tarjan 算法压缩强连通分量，再对分量 DAG 做 Kahn 排序；同分量块按块号升序。这样即使接缝图有环，顺序也稳定。

缓存有效性通过输入内容哈希判断。块内容哈希包含：

- 当前块完整高程和权重
- 烘焙算子、缩放、材质混合、接缝松弛
- 当前块和相邻块使用的材质层参数
- 相邻块完整高程和权重
- 关联接缝约束
- 算法版本

因此无关块或无关层参数不会导致缓存失效。

## 7. 预算调度

`Budget`：

```go
type Budget struct {
    MaxParallelism       int
    PerThreadPixelBuffer int
    NormalMapMemoryBytes int64
}
```

单块估算：

```text
128 bytes/pixel + 4096 bytes
```

调度器行为：

- worker 数不超过 `MaxParallelism`。
- 每个 worker 在执行前获取全局内存额度，完成后释放。
- 内存不足时阻塞在条件变量上。
- 单块估算超过总预算时立即返回 `E_BUDGET`。
- 块像素数超过每线程缓冲时按二维瓦片计算；即使缓冲为 1 像素也能完成。
- 排队原因由稳定任务序计算并写入 `QueueRecord`，不依赖实际唤醒时间。

调度记录用于诊断，不参与像素输出。

## 8. SQLite 检查点

数据库启用 WAL 和 `synchronous=FULL`。写入统一经过 `Store.mu`，每个块使用独立事务，批次内按稳定顺序调用。

### 表 `blocks`

关键列：

```text
block_id, revision, content_hash,
normal_hash, slope_hash,
parameter_version, algorithm_version,
dependencies, seam_stats,
width, height, origin_x, origin_y, bbox,
normals, slopes, raw_normals, raw_slopes,
commit_seq
```

法线/坡度以 little-endian `float64` gzip blob 保存。摘要通过 SHA-256 计算，加载时重新验证。

### 表 `commits`

每次提交记录：

```text
seq, block_id, revision, content_hash,
normal_hash, slope_hash,
parameter_version, algorithm_version, committed_at
```

`LatestCommitOrder(revision)` 对每块取该 revision 的最后一次 seq，再按 seq 排序。这样：

- 正常批次顺序稳定。
- 断点恢复可能重复提交已存在块，但最终每块最后顺序仍一致。
- 全量、增量、恢复、串行路径可以比较同一版本的归一化提交序列。

### 表 `meta`

保存 revision、工程哈希、参数版本、算法版本、`complete` 和快照拓扑序。成功提交后一次性写入 `complete=true`。

### 启动校验

启动加载时逐块检查：

1. 参数版本一致
2. 算法版本一致
3. 块属于当前工程
4. 内容哈希等于当前期望值
5. 法线/坡度 blob 摘要一致
6. 像素维度一致

任一失败则清空 `blocks/commits/meta`，引擎全量重算，并在报告中标记 `SnapshotInvalidated=true`。

## 9. 一致性比对

`compare.Snapshots` 按块号升序比较：

- 法线三分量位模式
- 坡度位模式
- 包围盒位模式
- 每条接缝原始/最终最大夹角
- 每个违反像素的坐标、夹角和归因

首个差异返回 `E_DIFF`，错误值包含：

```text
blockId, field, pixel, left, right
```

## 10. 三条路径验收

`internal/verify` 执行以下独立路径：

1. **全量**：最终编辑工程从空数据库一次烘焙。
2. **实时增量**：初始工程全量后逐条编辑，按失效闭包重烘。
3. **断点恢复**：同一增量数据库关闭并重开，只加载版本匹配块。
4. **中断恢复**：全量提交 2 块后模拟强制终止；重新打开后恢复 2 块、重烘 4 块。
5. **串行**：同一最终工程以 `MaxParallelism=1` 烘焙。
6. **失败隔离**：已提交快照上注入块 2 故障，确认快照未变化；清除故障后重试 4 块并复用 2 块。

所有成功路径比较像素、坡度、包围盒、接缝统计和每 revision 最后提交顺序。

## 11. 测试矩阵

`go test ./... -count=1` 覆盖：

- 样本生成确定性和工程往返
- `E_SCHEMA` / `E_SIZE` / `E_WEIGHT` / `E_PARA`
- 单块预览/精确/并发逐位一致
- 解析真值法线与坡度
- 非法算子、缩放、像素缓冲
- 增量失效范围与零重烘计数
- 全量与增量逐像素一致
- 接缝 `E_SEAM` 不提交
- SQLite 版本不匹配整体失效与全量重建
- 内存预算排队、峰值不超限、过小预算不丢块
- `E_DIFF` 首个块号/像素定位
- 失败隔离与重试
- 端到端三路径验收

## 12. 当前环境验证记录

核心：

```text
go version go1.22.12 linux/amd64
go test ./... -count=1                  PASS
terrainctl validate                     PASS
terrainctl verify                       PASS (12 checks)
```

Wails 环境：

```text
wails version                           2.10.2
Node/npm                                missing
GTK dev headers                         missing
WebKitGTK dev headers                   missing
host gcc wrapper                        target compiler missing
```

`CGO_ENABLED=0 wails build -platform windows/amd64 -nosyncgomod -s -skipbindings -nopackage` 已实际成功，生成 13,301,248 bytes 的 PE32+ GUI 可执行文件。默认 Linux 目标因宿主 gcc/WebKitGTK 缺失停在 `runtime/cgo`；这是 WSL 桌面工具链限制。

## 13. 已知限制

- 样本分辨率用于确定性技术验证，不是生产地形规模。
- 接缝松弛是边界法线后处理，不修改高程；真实几何折角会保留归因但被显示连续性处理。
- 单块结果整体驻留内存；超总预算的单块直接返回 `E_BUDGET`，未实现磁盘 spill。
- Wails 桌面窗口未能在当前 WSL 环境启动；后端绑定和静态前端可直接构建于完整桌面工具链。
