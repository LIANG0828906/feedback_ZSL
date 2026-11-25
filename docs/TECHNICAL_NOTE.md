# MeshDoctor 技术说明

## 1. 范围与数据流

MeshDoctor 是一个纯 Go 实现的确定性三角网格修复、拓扑验证与本地诊断工具。首次运行时，固定种子 `424242` 的生成器在工作目录写出：

- `samples/tetra.input.json`：重复顶点、翻转面、非流形重复面、三角小孔。
- `samples/cube.input.json`：重复顶点、翻转面、非流形重复面、四边形孔、自相交细长顶点位移。
- `samples/octa.input.json`：重复顶点、翻转面、非流形重复面、三角小孔、自相交细长薄片。
- `truth/*.manifest.json` 与 `truth/manifests.json`：原始闭合曲面的顶点数、面数、边数、欧拉特征、已知点、孔环坐标和缺陷标注。

六个阶段严格按以下顺序提交：

1. `diagnostic`：索引、退化面、清单字段和初始拓扑诊断。
2. `weld`：容差空间合并、退化面/重复面清理、邻接重建。
3. `fill`：边界环追踪、凹凸性与法向连续性耳切三角化、闭合/非流形校验。
4. `orient`：最大边连通分量 BFS 传播相邻面方向，并以质心方向确定根面朝外。
5. `smooth`：确定性邻接排序的同步拉普拉斯平滑；孔环接缝顶点固定。
6. `stats`：最终 V/F/E/χ、边界边、非流形边和真值校验。

## 2. 确定性与逐字节一致

所有路径使用同一个引擎和同一组参数。网格的比较/哈希不使用 JSON、`map` 遍历顺序或时间戳，而使用 `mesh.Mesh.CanonicalBytes()`：

- 固定魔数和版本；
- 小端 `uint64` 顶点数、面数；
- 每个 `float64` 输出 `math.Float64bits`；
- 顶点坐标和面索引按确定顺序写入。

因此实时分段、断点恢复和一次性全量重算在相同输入、编辑序列和参数下产生相同的阶段网格字节、最终哈希、拓扑统计和提交序列。比较器 `pipeline.CompareResults` 定位：

- 首个不一致阶段；
- `stage_sequence`、`mesh_length`、`vertex_bytes`、`face_bytes`、`stats` 等差异类型；
- 首个字节偏移；
- 可换算时的顶点索引或面索引；
- 两侧哈希、V/F/字节数摘要。

不一致由 `DiffReport.AsError()` 包装为 `E_DIFF`。

## 3. 状态机与版本

阶段状态只有 `pending`、`running`、`committed`、`failed`。合法迁移为：

- `pending -> running`
- `running -> committed | failed | pending`
- `failed -> running`

`committed -> running` 被拒绝并返回 `E_STATE`，已提交快照不会被同版本静默覆盖。参数版本由焊接容差、拉普拉斯权重和迭代次数的规范化 JSON SHA-256 派生，形如 `pv-xxxxxxxxxxxxxxxx`；算法版本固定为 `mesh-algo-1.0.0`。恢复时若快照参数版本、算法版本、快照内嵌版本或内容哈希与当前预期不匹配，存储层整体删除该样本的检查点链并全量重算；显式混版调用返回 `E_VER` 并指出阶段标识。

## 4. SQLite 检查点与强杀边界

CGO 启用时使用 `github.com/mattn/go-sqlite3`（驱动名 `sqlite3`）；`CGO_ENABLED=0` 时自动改用 `modernc.org/sqlite`（驱动名 `sqlite`），便于在没有 C 编译器的机器上构建。两个版本均锁定在 `go.mod`。

`Store` 通过以下措施保证提交边界：

- `database/sql` 最大打开连接数为 1；
- 存储层互斥锁串行化写；
- 每个阶段的 `mesh_blob`、`mesh_hash`、`param_version`、`algo_version`、参数 JSON 和完整快照 JSON 在同一个 SQLite 事务中写入，同时把阶段状态更新为 `committed`；
- 已存在但哈希不同的同版本检查点按冻结冲突拒绝。

开始执行阶段前，状态会先持久化为 `running`。若进程在计算中或事务提交前被 `SIGKILL`，重启会看到陈旧 `running`，按 `running -> failed -> running` 归一化后重试；已经事务提交的阶段从 SQLite 恢复，未提交阶段重算。工具不会把强杀后未提交的阶段声称为完整报告。

## 5. 真值与接缝校验

`pipeline.CheckTruth` 在每个阶段后计算：

- 已知点到当前网格最近三角形的距离和面索引；
- 真值孔环每条边的采样点到当前网格的最大曲面距离，作为闭环闭合误差。

已知点超差返回 `E_POINT`，详情含 `point_id`、实测距离和容差。孔环断裂返回 `E_LOOP`，详情含 `ring_id`、声明环坐标、最差线段和误差。为保证接缝在平滑后仍可逐字节追踪，平滑阶段固定填孔环顶点；非接缝已知点容差按拉普拉斯收缩幅度显式声明。

## 6. 并发资源边界

`pipeline.BoundedPool` 使用两个信号量：

- worker token 限制最大并行度；
- buffer token 数量为 `maxParallelism * bufferMeshesPerWorker`，代表可占用的网格缓冲预算。

预算耗尽时 `Submit`/`AcquireBuffer` 阻塞排队，不丢弃任务。样本结果写入预分配下标，最终按输入顺序返回；测试 `TestBoundedParallelMatchesSerial` 验证并行和串行的每个阶段字节一致。

## 7. 本地诊断界面

启动：

```bash
go run ./cmd/meshdoctor serve --addr 127.0.0.1:18080
```

页面和 API 均为同源本地资源，不引用外部 JS/CSS/账号。页面提供：

- Canvas 2D 原网格/阶段网格叠加；
- 真值孔环和缺陷坐标高亮；
- 六阶段状态矩阵；
- 三路径比对和首个差异定位；
- SQLite 缓存命中/未命中/作废统计；
- 样本、路径、阶段选择；
- 增量运行/重试、取消请求；
- 阶段 JSON 和 OBJ 导出。

主要 API：

- `GET /api/state`：清单、报告、运行状态。
- `GET/POST /api/verify?parallel=1`：执行三路径和错误契约检查。
- `POST /api/run?sample=cube&weight=0.5&iterations=2`：后台增量/恢复运行。
- `POST /api/cancel?sample=cube`：请求在下一阶段边界取消。
- `GET /api/export?format=obj|json|report&sample=...&path=resume&stage=5`。

## 8. Windows 说明

诊断服务默认监听 `127.0.0.1:18080`，Windows 浏览器可直接打开。若使用 CGO 版 `mattn/go-sqlite3`，需安装 MSYS2/MinGW 并令 `gcc.exe` 在 `PATH`；若暂时没有 C 编译器，可使用：

```powershell
$env:CGO_ENABLED="0"
go run ./cmd/meshdoctor serve
```

该构建使用纯 Go SQLite 驱动。已验证 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/meshdoctor` 可生成 `.exe`。
