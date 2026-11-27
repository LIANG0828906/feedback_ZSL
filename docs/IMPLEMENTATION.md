# 弹道预测与障碍反弹网格内核 — 技术说明

首版在空目录中独立成形，只依赖 Go 1.22 标准库。领域根包名为 `grid`（模块 `ballisticgrid`），可直接被其他 Go 组件导入；`cmd/ballisticgrid` 提供本地可重复的验证、诊断 HTTP 与强杀恢复演示。

## 一键验证

```bash
go test ./... -count=1
go test -race ./... -count=1
go run ./cmd/ballisticgrid verify --dir artifacts/run
go run ./cmd/ballisticgrid serve --addr 127.0.0.1:8088 --dir artifacts/run
```

也可使用 `make verify`、`make test`、`make race`、`make serve`。

`verify` 会确定性生成并写出：

- `schema_v1.json` / `schema_v2.json` / `schema_v3.json`
- `requests.json`：按逻辑帧排列的 5 条预测请求
- `truth.json`：首次命中、反弹点、反弹序号、逃逸/反弹上限真值
- `tiles.db`：经 `database/sql` 和仓库内置 `sqlite` 驱动提交的分块快照
- `summary.json` / `restart.json`：三路径、失效传播、并发等待和异常码报告

## 统一领域口径

全链路使用整型定点值，不做浮点累积：

| 量 | 内部口径 |
| --- | --- |
| 坐标/端点 | 微米，`1 mm = 1000` 内部单位 |
| 时间 | 毫秒；v1 的一个 tick 按声明为 `10 ms` |
| 反弹系数 | 万分之一（basis points） |
| 法线 | 单位长度 `1000`，允许生成器舍入后的 ±5‰ 模长误差 |
| 扫掠命中时间 | `1/1024` 帧定点分数 |

求交的叉积、反射分子等易溢出运算用 `math/big` 做精确整数中间值，再量化回 int64；这避免浮点误差，也能在导出换算溢出时返回带两个操作数的 `E_OVF`。对外覆盖率比例只是整数计数 `/1024` 的展示值，不参与物理或哈希决策。

## 三版本 Schema

| 能力 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 版本字段 | `schema_version` | `schema` | `schemaLevel` |
| 障碍列表 | `obstacles` | `obstacleList` | `barriers` |
| 端点 | `start_mm` / `end_mm`，`x_mm/y_mm` | `from` / `to`，`x/y` | `anchorDelta` / `endpointDelta`，相对上一障碍起点和自身起点的增量 |
| 法线 | 缺失，升级补右手法线默认值 | `normal {x,y}` | `face {x,y}` |
| 反弹 | `restitution_pm` 千分比 | `restitutionPermille` 千分比 | `bounceBasisPoints` 万分比 |
| 时间 | `time_step_tick` | `timeStepMs` | `stepMillis` |
| 穿透 | 缺失，补 `false` | 缺失，补 `false` | `allowsPenetration` |

关键 API：

- `grid.Generate(20261127)`
- `grid.DecodeVersion([]byte)`
- `grid.ExportVersion(level, version)`
- `grid.VerifyExportRoundTrip(artifact)`
- `grid.FirstContentDifference(expected, decoded)`

每次补字段都会产生 `FieldTrace`，包含对象 ID、字段路径、旧值 `<absent>`、新值和来源默认值。降级时 `ConversionReport.Losses` 给出 `E_LOSSY` 清单，记录法线/穿透字段无法表达、v3 增量端点折叠为绝对端点等处置；若原值不是声明默认值，`SemanticLoss=true`。导出产物按“降级归一化模型”校验逐字段往返；若拿原始高版本模型比较且存在真实语义丢失，会返回首个不等字段的 `E_ROUND`。

## 物理、分块与版本

- `grid.Predict`：重力冲量 → 帧位移 → 整数叉积扫掠求交 → 最早命中 → 定点反射 → 剩余帧时间继续求交。
- 每个反弹记录请求、帧、序号、障碍 ID、坐标、入射/出射速度。
- `FrameReport.Traces` 逐帧记录重力、位移、命中坐标、剩余时间和反射分量的截断位置、分子/分母与余数。
- `grid.ProjectScene` / `ProjectSceneWithPool`：默认正交相机 `250 mm/px`、视口 `128×64 px`、分块 `32×32 px`，共 4×2 块；用整数像素中心距离判定障碍/折线/反弹覆盖。
- 分块哈希包含覆盖掩码、投影像素记录、障碍 ID 和材质版本；因此只依赖内容，跨进程重跑一致。
- `SemanticVersions` 独立维护端点、法线、反弹、重力、时间步和结构版本。缓存键为 `SHA256(contentHash | structureVersion | cameraVersion | algorithmVersion)`。
- `grid.EditObstacle` 同时投影编辑前/后场景，合并直接障碍分块与反弹后下游路径分块，输出 tile 坐标、旧版本分量和失效依据。

## SQLite 快照与恢复边界

仅用标准库意味着没有引入外部 SQLite 驱动。`internal/sqlitedrv` 注册名为 `sqlite` 和 `grid-sqlite` 的 `database/sql/driver`，实现本项目使用的 SQLite 风格 `CREATE TABLE`、`INSERT OR REPLACE`、按主键/顺序 `SELECT` 子集。数据以单文件表 JSON 持久化，提交时写 `tiles.db.tmp` 再原子 `rename`；上层 `SnapshotStore` 使用 `SetMaxOpenConns(1)` 加互斥锁，保证分块事务串行、顺序稳定。

每个分块在一个事务中整体写入：

- 颜色缓冲与深度缓冲 BLOB
- 颜色/深度摘要
- 分块内容哈希
- 结构版本、相机版本、算法版本
- 完整语义版本 JSON、覆盖率和命中统计

重启后 `CachedTile.Recoverable` 比较新投影内容哈希和缓存键版本分量；不匹配即视为失效并重算。`TestForcedKillRecoversOnlyCommittedSnapshots` 会真实构建 CLI、启动 `crash-demo`、发送 `SIGKILL`，随后只读到已提交的 3 个分块；强杀路径不生成退出报告。未提交事务回滚由 `TestSnapshotRollbackAndStaleHashInvalidation` 覆盖。

`grid.RunWithCache` 对可复用快照执行逐像素比对；`grid.CompareScenes` 对全量空缓存路径、重启续算路径和增量编辑路径比较颜色、深度、覆盖率、内容哈希和命中统计。首个不一致分块/字段返回 `E_DIFF`。

## 并发边界

`grid.RayPool` 默认最大并行度 4、每 worker 射线缓冲 4：

- worker ID 令牌桶限制最大并行度；槽位满时阻塞并记录 `parallel-slot-full` 等待时长。
- 每 worker 独立带容量 ray channel；缓冲满时阻塞并入队，记录 `ray-buffer-exhausted`，不丢弃射线。
- `Flush(workerID)` 等待该 worker 的射线完成；统计中的 `completed` 可核对无丢失。

## 错误码与注入

| 码 | 触发 | 定位字段 |
| --- | --- | --- |
| `E_DECODE` | 缺字段、重复障碍 ID、法线非单位 | object + JSON field path |
| `E_LOSSY` | 降级无法表达的表示/非默认可选值 | `ConversionReport.Losses` |
| `E_ROUND` | 往返字段不等 | 首个 field |
| `E_OVF` | 整数换算溢出 | field + 两个 operands |
| `E_BOUNCE` | 反弹序号超过上限 | requestId + object + bounceSeq |
| `E_COVER` | 分块覆盖率超过声明上限 | tile + measured |
| `E_DIFF` | 多路径像素/哈希/统计不一致 | tile + field |
| `E_ORDER` | 端点生效帧时序倒置 | OB + tile + revision field |
| `E_NORM` | 单位法线与线段不垂直 | OB + tile |
| `E_BLOCK` | 单分块内部失败注入 | tile + obstacle |

诊断页按钮调用 `/api/inject?kind=decode|duplicate|order|norm|block|bounce|cover`，首页 SVG 展示分块覆盖、折线路径、反弹点和红色虚线失效分块。

## 实测结果（2026-11-27，WSL Ubuntu / Go 1.22.12）

- `go vet ./...`：通过，无输出。
- `go test ./... -count=1`：根包 20 个测试与 `internal/diag` 测试全部通过。
- `go test -race ./... -count=1`：全部通过。
- `go run ./cmd/ballisticgrid verify --dir artifacts/run`：
  `tiles=8 recovered=3 reused=5 recomputed=3 poolPeak=4`，三路径一致。
- HTTP 实测：`/healthz` 返回 `ok`；`/api/inject?kind=norm` 返回 `E_NORM` 且定位 `OB01` / `(1,1)`；`/diagram.svg` 返回 `200 image/svg+xml`。

不同机器上的并发等待纳秒/毫秒数会随调度变化，但分块哈希、像素、覆盖率、失效分块坐标和提交顺序保持确定。
