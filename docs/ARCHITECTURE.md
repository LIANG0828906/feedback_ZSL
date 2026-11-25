# 弹道反弹与分块校验内核

本仓库是一个无外部服务依赖的 Go 1.22 模块，包含可集成领域内核、SQLite 快照层、失效传播编排和本地诊断工作台。

## 包结构

- `internal/kernel`：三版本 schema、确定性生成器、定点数学、扫掠求交/反射、真值、相机投影、分块哈希、确定性渲染和有界 worker pool。
- `internal/snapshot`：`modernc.org/sqlite` 纯 Go 驱动实现的版本化分块快照库。
- `internal/workbench`：编辑、语义版本差异、精确失效传播、增量/全量执行和帧比对。
- `internal/diagnostic`：`net/http` 回环服务与内嵌零依赖工作台页面。
- `cmd/diagnostic`：本地演示入口。

## 运行

```bash
go test -count=1 ./...
go run ./cmd/diagnostic --addr 127.0.0.1:18080 --db .diagnostic/ballistic.sqlite
```

浏览器打开输出中的回环地址。若端口被占用，可改用 `--addr 127.0.0.1:0`，启动日志会打印实际端口。

## 三版本 Schema

| 版本 | 端点 | 反弹系数 | 时间 | 法线/穿透 |
| --- | --- | --- | --- | --- |
| v1 | `x1/y1/x2/y2` 绝对坐标 | `bounce_permille`，千分比 | `step_ticks × ms_per_tick` | 均缺失，补 `left` 与 `false` |
| v2 | `start_delta` + `vector_delta` 增量链 | `restitution_basis_points`，万分比 | `step_milliseconds` | 有 `normal_facing`，穿透缺失时补默认 |
| v3 | `endpoints.a/b` 绝对坐标 | `restitution.per_ten_thousand`，万分比 | `time_step_ms` | 有 normal/penetration/material |

加载错误使用 `E_SCHEMA`，并携带 `$.path.to.field`、来源版本和标识。JSON 损坏返回 `E_DECODE`；时间序列倒置返回 `E_ORDER`；参数越界返回 `E_PARA`；分块内部失败返回 `E_BLOCK`；定点运算溢出返回 `E_OVF`。

降级导出返回 `ExportResult.Transcript.Losses`，其中每条记录的 `code` 为 `E_LOSSY`，并给出字段、旧值、默认值和处置方式。v1 无法显式表达法线、穿透和材质，导出时记录 `omit_..._restore_default_on_load`；千分比换算余数按声明的 nearest-half-away 规则处理。

## 定点与物理

- 位置：`PosScale = 1024` 内部定点单位/世界单位。
- 反弹系数：`CoeffScale = 10000`。
- 时间：内部以毫秒步长参与积分，`ms_per_tick` 负责 tick/ms 整数换算。
- 乘法/除法中间值使用 `math/big` 防止 int64 中间积溢出；可表示范围内再转回 int64。
- 每帧记录重力量化余数、位移量化余数、求交余数和反射量化余数。
- 扫掠求交使用线段参数方程和精确符号比较；反射使用未归一化法向量的有理数公式，避免浮点累积。

## 分块、哈希与渲染

默认相机为 `960×576`、tile 为 `64×64`，共 15×9=135 块。投影、Bresenham 线、coverage mask、颜色和深度缓冲均为整数确定性计算。

`ContentHash = SHA256(geometry summary + obstacle material id/version + restitution + masks + bounce markers)`，只包含该 tile 的局部内容。同一 process、同一场景和相机下哈希稳定；无关 tile 的输入不变，因此哈希不变。

每块输出：

- `coverage_bp`：障碍/折线/反弹标记并集占 tile 像素比例。
- `obstacle_coverage_bp`、`polyline_coverage_bp`。
- `polyline_share_bp`：该 tile 折线像素占全部折线像素比例。
- `color_digest`、`depth_digest`。

## 版本、失效与缓存

语义版本分量包括 `endpoints`、`normals`、`restitutions`、`gravity`、`time_step`、`materials`。`structure-vN` 表示 segment/request 集合与顺序等拓扑结构；数值语义进入内容哈希和 `semantic_version_key`。

缓存键由以下字段组成：

```text
tile id | content hash | scene structure version | camera version | algorithm version
```

SQLite `tile_current` 保存当前快照，`tile_commit_log` 保存串行提交日志。每个 tile 在一个事务中同时写 current/log，包含颜色摘要、深度摘要、内容哈希、结构版本、相机版本、算法版本、覆盖率和命中统计。最大连接数为 1，编排层按 tile 顺序提交，因此提交顺序稳定且不交错。

重启时若内容哈希、结构版本、相机版本或算法版本不匹配，会删除该 current 行并按需重算。

## 精确失效传播

编辑后重新预测和建图，并比较旧/新 tile 哈希：

- 端点编辑：旧/新线段投影 tile 为直接失效集；只有旧/新命中该线段或归因发生变化的弹道才进入归因失效。
- 反弹系数编辑：线段材质/系数 tile 立即变化；所有命中该线段的弹道从反弹点开始的下游 tile 标记失效。
- `InvalidationPlan` 输出版本分量 before/after、direct/downstream/changed-hash/invalid/reused tile 和 attribution IDs。
- 未失效 tile 从 SQLite 读回 RGBA/depth buffer 与摘要，不重新渲染。

增量帧与空内存库全量帧通过 RGBA 字节、color/depth digest、coverage、content hash 和 bounce/hit stats 比较；`FrameComparison` 给出第一个不一致像素和 tile。

## 并发边界

`BoundedPool` 默认最多 2 个 worker，单任务几何缓冲上限 256 KiB。获取不到 worker 时阻塞排队，并写入 `worker_pool_exhausted` 审计事件；超过缓冲上限的任务直接返回 `E_BLOCK`。SQLite 提交仍由单 goroutine 按稳定顺序执行。
