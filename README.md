# spatialpick

面向游戏编辑器与互动媒体场景的确定性拾取、空间索引与枢轴操作组件库。仓库内同时包含：

- 可集成 Go 包 `spatialpick`
- 本地确定性场景/真值生成器 `cmd/scenegen`
- 完整可运行示例 `cmd/demo`
- 加载校验、纯函数查询、定点传播、依赖失效、事务、快照、三路径比对测试

无第三方依赖，使用 Go 1.22+。不依赖 Docker。

## 快速开始

```bash
go version
go test ./...
go run ./cmd/scenegen -out data
go run ./cmd/demo \
  -data data \
  -snapshot data/editor.snapshot \
  -checkpoint 2
```

竞态与性能验证：

```bash
go test -race ./...
go test -run '^$' -bench . -benchmem .
```

Windows 下请确保 `data`、快照所在目录和模块目录可写。快照采用同目录临时文件 + `fsync` + 原子重命名；Unix 使用 `flock`，Windows 使用独占锁文件与过期锁清理。`cmd/demo` 会实际写入 `data/editor.snapshot`，因此目录必须具备持久化写权限。

最小集成方式：

```go
scene, _, err := spatialpick.LoadSceneWithTruth("data/scene.json", "data/truth.json")
engine, err := spatialpick.NewEngine(scene)

_, err = engine.PickRay(scene.RayQueries[0])
_, err = engine.SelectBox(scene.BoxQueries[0])

plan, err := engine.PlanPivotBatch(scene.EditSequence[0])
_, err = engine.CommitBatch(plan) // 失败时状态保持不变

ctx := context.Background()
err = engine.SaveSnapshot(ctx, "data/editor.snapshot")
restarted, status, err := spatialpick.OpenEngineWithSnapshot(ctx, scene, "data/editor.snapshot")
left, right := engine.Bundle(), restarted.Bundle()
err = spatialpick.CompareBundles(&left, &right)
```

## 交付结构

| 文件 | 作用 |
|---|---|
| `validate.go` | 场景/真值文档加载与全量结构校验 |
| `fixed.go`, `derived.go` | int64 定点矩阵、逐步传播、误差轨迹、世界包围体 |
| `index.go`, `query.go` | 持久化可复用 BVH、射线/框选/点包含纯函数查询 |
| `graph.go`, `engine.go` | 依赖图、精确失效、事务提交、并行增量重建 |
| `geometry_edit.go` | 运行时几何体增删/替换及其精确失效 |
| `snapshot.go`, `lock_*.go` | 完整状态快照、校验、原子串行发布 |
| `compare.go`, `paths.go` | 首个差异定位、增量/断点/全量三路径验证 |
| `generator.go` | 确定性样本与解析真值生成 |
| `data/scene.json`, `data/truth.json` | 已生成样本与真值对 |

## 数据与语义

### 场景

`Scene` 包含相机、几何体数组、节点树、查询请求和编辑批次。节点使用：

```json
{
  "id": "n002",
  "parent_id": "n001",
  "local_matrix": [1, 0, 0, 0.1, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1],
  "geometry_index": 1
}
```

几何体类型：

- `aabb`：`min` / `max`
- `sphere`：`center` / `radius`
- `mesh`：`vertices` / `triangles`

相机是正交相机。屏幕射线：

```text
pixelsPerWorld = viewport_height / world_height
ndcX = (screen.x - viewport_width/2) / pixelsPerWorld
ndcY = (viewport_height/2 - screen.y) / pixelsPerWorld
origin = eye + right*ndcX + up*ndcY
dir = normalize(forward)
```

框选语义为“节点世界 AABB 的屏幕投影与矩形重叠”。点包含语义：

- AABB 使用局部 AABB；
- Sphere 使用变换后的局部球；
- Mesh 使用其世界包围体（局部顶点 AABB 变换后）。

### 真值

真值由生成器构造场景时直接计算并写入 `truth.json`，不从组件查询结果反推。包括：

- 每个节点局部矩阵摘要；
- 每条射线与每个几何体的首个交点及全局首交点；
- 每个框选查询的完整命中集合；
- 每个点查询的命中集合；
- 每批枢轴操作后的所有节点世界矩阵；
- 编辑后再次执行全部查询的真值；
- 故意构造的球面切触、框选边缘和点贴边模糊位置。

确定性样本规模：61 节点、31 几何体、最长链 48 层、12 射线、5 框选、2 点查询、4 个枢轴编辑批次。

## 定点变换与误差

所有公开世界矩阵为行主序 int64 定点矩阵，精度由场景 `precision` 声明，量化单位为：

```text
q = 10^-precision
```

每次父子乘法都按 q 舍入，并记录：

- 当前节点、深度；
- 本步量化误差；
- 本步新增上界；
- 累积矩阵误差上界；
- 世界矩阵无穷范数。

派生误差采用线性深度上界：

```text
step = q * (0.5 + 0.5*||local||∞ + 0.5*||parentWorld||∞)
cumulative = parentCumulative + step
geometryBound = cumulative*(3 + 3*localSize + 3*maxAbsLocalCoord) + q*(depth+2)
```

因此误差随层级深度、量化步长和几何尺寸单调有界。查询再叠加 `q*(depth+1)` 作为最终判定上界。

固定量化值只在精确判定后发布。落在上界内时：

- 射线/点/框选查询返回 `E_AMBIG`；
- 错误中带 `query_id`、`node_id`、`geometry_id`、`depth`、原因和边界值；
- 结果对象同时保留候选命中与完整模糊位置列表；
- 不会静默选择一侧，也不会通过调高声明精度掩盖情形。

## 索引与查询

索引为不可变 BVH：

- 构建时按最长质心轴中位切分；
- 每个叶节点对应一个“节点 + 几何体”条目；
- 变换更新只复制受影响条目的叶到根路径，其他子树与旧索引共享；
- 几何体增删导致结构变化时只重建 BVH 条目/树，不重算未受影响节点的世界派生；
- 同一 `*Index` 可由任意 goroutine 并发查询。

查询不读取历史状态、线程 ID 或全局变量。命中节点按 ID 稳定排序；射线首交点按 `t`、节点 ID、几何体 ID 稳定决胜。

参数越界返回 `E_PARA`，包括屏幕坐标/框选矩形超出视口和非法相机。

## 依赖失效与事务

依赖图显式记录：

- 局部变换 -> 世界缓存
- 父世界缓存 -> 子世界缓存
- 节点世界缓存 -> 索引条目
- 几何体 -> 派生缓存/索引条目

变换失效范围是“变动节点 + 全部后代”的派生缓存和索引条目。非后代缓存指针原样复用。`PlanTransformInvalidation` 输出对象 ID 与原因；提交时重新计算必需范围，计划漏项返回 `E_DEP` 并列出推定遗漏对象。

几何体增删/替换通过 `ApplyGeometryMutations` 执行，只使对应节点的派生缓存和索引条目失效，不影响子节点世界变换。

枢轴批量编辑：

1. `PlanPivotBatch` 在只读状态上计算工作副本；
2. 平移、旋转、缩放均围绕世界枢轴点组合；
3. 选中节点转换为新的局部矩阵；
4. `CommitBatch` 检查基础 revision/index version；
5. 在旁路状态上完成传播和索引更新；
6. 只有全部成功才一次替换状态。

提交过期、矩阵非法或任一步失败均返回 `E_TXN`，带批次与对象标识，原状态不变。

## 并行增量重建

`RebuildSubtree`：

- 旧状态保持可读，查询可与重建并行；
- 按深度层推进，同层节点用有界 worker 池并行；
- `MaxParallel` 默认 CPU 数并限制到 32；
- `MaxScratchBytes` 在启动前估算，超限返回 `E_RESOURCE`；
- 单节点异常仅将该节点及受影响后代标为失败，其他条目继续可用；
- 失败条目查询/读取返回 `E_STALE`；
- 再次调用即可重试恢复；
- 计算结束后若状态 revision 已变化，本次结果不发布，返回 `E_TXN` 要求重试。

共享索引状态替换在写锁内完成；快照提交另由引擎互斥量与文件锁串行化。

## 三路径一致性

`RunThreePaths` 对同一编辑序列执行：

1. 增量路径：逐批计划/提交；
2. 断点恢复路径：提交到 checkpoint，保存快照，重新加载后继续；
3. 全量派生重建路径：从最终局部变换重建全部世界缓存、包围体和索引。

`CompareBundles` 逐项比较：

- 节点深度、int64 世界矩阵、AABB、世界包围球、误差边界；
- 射线首交点、逐几何体交点、错误码；
- 框选/点查询命中集合；
- 所有 `E_AMBIG` 的查询/节点/几何体/深度位置。

首个不一致返回 `E_DIVERGE`，带 `object_id`、`data_item`、`left`、`right`。定点矩阵和模糊位置要求完全一致，浮点派生在 4q 容差内。

## 快照协议

快照是单文件信封：

```json
{
  "format_version": 1,
  "checksum": "sha256(payload bytes)",
  "payload": "<base64 encoded JSON payload>"
}
```

payload 包含：

- 快照格式/版本；
- 基础场景哈希、结构哈希、场景版本、精度；
- 当前内容哈希、revision、index version；
- 已提交批次；
- 全部定点局部矩阵与各节点版本；
- 全部派生缓存（世界/逆矩阵、AABB、包围球、误差轨迹、失败状态）；
- 有效几何体覆盖（含运行时增删）；
- 全部索引条目、BVH、误差上下界。

加载时任何一项不匹配都会整份丢弃并全量重建，不做旧/新条目部分混用。写入先写同目录临时文件，`fsync` 后原子重命名；读取方只会看到完整旧版本或完整新版本。取消或强制终止最多留下临时文件，不会替换已发布快照。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_SCHEMA` | 字段缺失、类型/数值非法、内容哈希或真值摘要不符 |
| `E_CYCLE` | 父子引用成环 |
| `E_REF` | 父节点、几何体索引、三角索引或查询引用越界 |
| `E_PARA` | 查询参数越界或相机非法 |
| `E_STALE` | 派生数据版本过期或条目处于失败状态 |
| `E_AMBIG` | 判定落在声明的误差上界内 |
| `E_DEP` | 失效计划漏掉实际依赖对象 |
| `E_DIVERGE` | 两条验证路径首个对象/数据项不一致 |
| `E_TXN` | 批次基础版本过期、矩阵非法或提交失败 |
| `E_NUMERIC` | 定点运算溢出/奇异 |
| `E_RESOURCE` | 并行重建工作集超过内存上限 |
| `E_CORRUPT` | 快照文件不可读 |

所有领域错误为 `*spatialpick.CodedError`，可用 `spatialpick.IsCode` 和 `errors.As` 判断。

## 已知边界

- 样本相机为正交相机；屏幕射线接口没有引入透视投影。
- 框选使用投影后的世界 AABB，而非逐三角精确裁剪。
- Mesh 点包含使用世界包围体语义，不声明为实体网格内部判定。
- 几何体增删会重建索引树结构，但不会重算未受影响节点的世界派生。

实际测试、示例输出和基准结果见 `VALIDATION.md`。
