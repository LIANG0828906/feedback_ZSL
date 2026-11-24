# 地形遮挡剔除与流式加载技术验证

本仓库是一个无外部地形数据、无网络样本依赖的 Go 1.22 原型。固定种子
`420261125` 生成 `17×13` 高度场顶点、`16×12` 单元、`12` 个连续覆盖分块、
`3` 个遮挡体和 `9` 个“推进—顶点回退”相机采样时刻。

SQLite 驱动固定为纯 Go 的 `modernc.org/sqlite v1.29.10`，因此不需要 C 编译器。

## 快速运行

Windows PowerShell：

```powershell
go version
go env
$env:CGO_ENABLED = "0"
.\scripts\verify.ps1
go run .\cmd\terrain-occlusion serve -in samples\project.json -artifacts artifacts -addr 127.0.0.1:8080
```

Linux/macOS：

```bash
./scripts/verify.sh
go run ./cmd/terrain-occlusion serve -in samples/project.json -artifacts artifacts -addr 127.0.0.1:8080
```

也可分步执行：

```bash
go build ./...
go vet ./...
go test -count=1 ./...
go run ./cmd/terrain-occlusion generate -out samples/project.json
go run ./cmd/terrain-occlusion verify -in samples/project.json -artifacts artifacts
```

## 代码结构

- `internal/model`：JSON 契约、诊断报告和稳定错误码。
- `internal/samplegen`：固定种子地形、分块、遮挡体、相机轨迹与独立解析真值预言。
- `internal/projectio`：样本读取、字段存在性、尺寸、覆盖、轨迹和引用校验。
- `internal/geo`：确定性向量/相机/光源数学与高度场阴影图光栅化。
- `internal/cull`：粗阴影判定、视锥 AABB、逐点深度区间和三态结论。
- `internal/estimate`：量化误差分量、累积上界和 `E_EST` 判定。
- `internal/stream`：加载槽位、内存预算、距离/成本优先级、排队和驱逐。
- `internal/parallel`：最大并行度和每线程深度缓冲上限。
- `internal/statemachine`：`pending → culling → loaded → committed/failed` 状态机。
- `internal/checkpoint`：SQLite 表、哈希链、原子串行提交和重启校验。
- `internal/runner`：单时刻推进、真值核对、冻结、恢复和参数版本隔离。
- `internal/diagnostic`：实时、断点恢复、一次性重放三路径编排与首个分歧定位。
- `internal/diag`：`net/http` API 和嵌入式原生 Canvas/SVG 诊断页。
- `cmd/terrain-occlusion`：`generate`、`verify`、`serve` 命令。

## 异常码

| 码 | 触发条件 | 验证 |
| --- | --- | --- |
| `E_SCHEMA` | 必需字段为 `null`/缺失，或成本等必填值非法 | `internal/projectio/projectio_test.go` |
| `E_DIM` | 高度场长度错误、ID 不连续、覆盖越界/重叠/面积不一致、时间不单调 | 同上 |
| `E_REF` | 分块或真值引用了不存在的遮挡体 | 同上 |
| `E_PARA` | 阴影分辨率、光源方位/高度角、采样间距、定点步长、视口或资源边界越界；错误含字段与允许区间 | 同上 |
| `E_VIS` | 运行时结论与真值分块结论不同；详情含 `chunk_id`、`truth`、`observed` 和两侧裕量/区间 | `internal/acceptance/acceptance_test.go` |
| `E_LOAD` | 可见块驻留延迟超过声明上限，或装卸序列偏离真值；详情含分块与延迟采样数 | 同上 |
| `E_EST` | 已观测集合差异超过由量化参数推出的上界；详情含采样时刻 | 同上 |
| `E_STATE` | 非法状态迁移；原状态保持不变 | 同上 |
| `E_HASH` | SQLite 哈希链、`project_hash` 或 schema 版本不匹配；整个快照作废 | `internal/checkpoint/store_test.go` |

## 误差界

设阴影分辨率为 `R`，归一化采样间距为 `s`，定点步长为 `δ`：

```text
ε_shadow   = 1 / R
ε_depth    = δ
ε_project  = δ × (1 + s)
ε_sample   = s / R
B          = ε_shadow + ε_depth + ε_project + ε_sample
U_t        = (t + 1) × B
K_t        = ceil(R × U_t) + streaming_delay_impact
```

运行时对解析裕量做 floor 定点量化得到 `q`，给出非对称保守区间
`[q-B, q+δ+B]`：

- 下界大于 `0`：`visible`
- 上界小于 `0`：`occluded`
- 区间跨过 `0`：`uncertain`

生成器的真值预言使用未量化几何裕量和同一保守阈值独立分类，不再直接调用
运行时的 `EvaluateChunk` 结论。`K_t` 只从已观测的 `R`、`s`、`δ`、采样序号和
实际延迟块数推出；若观测差异大于 `K_t`，`estimate.CheckSufficiency`
返回 `E_EST`。

## 流式策略

- 请求集合为严格可见块加保守的不确定块。
- 新请求按 `camera_distance + 0.05 × load_cost` 升序处理。
- 旧排队请求按等待年龄优先，同年龄仍按距离/成本排序。
- 每刻最多使用 `budgets.load_slots` 个完成槽位。
- 内存不足时先按距离降序驱逐非请求驻留块；仍无法容纳则记录
  `memory_budget_exhausted` 并排队。
- 槽位不足记录 `load_slots_exhausted`。
- 已不再可见的排队项进入 `cancelled_ids`，不会静默消失。
- 超过 `max_residency_delay_samples` 仍未驻留时返回 `E_LOAD`。

## 检查点与回退

SQLite 数据库包含 `meta` 和 `checkpoints` 表。每条检查点在一个事务中写入：

- 可见集合摘要 `visible_digest`
- 不确定块 JSON
- 驻留块索引 JSON
- 阴影参数版本
- 相机轨迹消费偏移
- 完整 `SampleResult`
- 可恢复流式规划器状态
- 前一哈希与本条哈希

Store 使用互斥锁且连接池最大连接数为 1，保证进程内提交串行、顺序稳定。
重启时 `LoadVerified(projectHash)` 校验 schema、项目绑定、连续 sample index、
`prev_hash` 链和内容哈希；任一路径损坏都返回 `E_HASH` 且不返回部分记录。

相机 `pose_key` 与阴影参数版本共同构成重访缓存键。相同版本的回退姿态直接
命中原已提交判定；参数版本变化后不会混用旧结论。已提交 `SampleResult`
是值快照，后续参数修改只进入新的采样时刻。

## 三路径与诊断页

`diagnostic.RunThreePaths` 产出：

1. `realtime`：连续实时推进并写 `artifacts/realtime-checkpoints.db`。
2. `resume`：先提交前 3 个时刻，关闭 DB，再校验哈希并恢复推进到末尾。
3. `replay`：全新 DB 上一次性全量重放。

比较器逐时刻核对可见、不确定、遮挡、驻留、逐块区间、加载事件和卸载事件；
失败时返回首个 `sample_index`、`chunk_id` 与字段名。

诊断页 `http://127.0.0.1:8080/` 展示：

- 高度场底色与分块着色预览
- 可见/遮挡/不确定分层开关
- 相机、目标点和姿态
- 光源阴影深度图
- 误差界、不确定块数和集合差异上界曲线
- 加载槽位、内存条、排队/延迟/资源等待
- 三路径哈希与首个分歧定位

页面所有脚本和样式均内嵌或由 `/api/diagnostic` 提供，不依赖 CDN。

## 实测结果

当前工作区实测环境：

```text
go version go1.22.12 linux/amd64
CGO_ENABLED=0
```

已通过：

```text
go build ./...
go vet ./...
go test -count=1 ./...
GOOS=windows GOARCH=amd64 go build ./...
```

验收命令实际输出：

```text
generated seed=420261125 chunks=12 camera_samples=9 -> samples/project.json
verdicts visible=8 occluded=80 uncertain=20 revisit_hits=3
three-path identical=true first_sample=-1 field=
report=artifacts/diagnostic-report.json
```

本地 HTTP 检查：

```text
GET /api/health        -> {"ok":true}
GET /                  -> 包含“地形高度场遮挡剔除”
GET /api/diagnostic    -> ok=true, comparison.identical=true, shadow_frames=6
```

`shadow_frames=6` 是因为 3 个回退姿态命中已提交结论，不重复生成阴影图。

说明：当前容器里的 `/home/liang/.local/bin/gcc` 包装器仍指向已不存在的旧
relay 工具链路径，直接使用默认 `CGO_ENABLED=1` 会在 `runtime/cgo` 阶段失败。
本项目选用纯 Go SQLite 驱动并验证 `CGO_ENABLED=0`；如果改用
`github.com/mattn/go-sqlite3`，则必须先安装可用的 GCC/MinGW C 编译环境。
