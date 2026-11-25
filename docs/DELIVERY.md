# 透明介质体积雾分块积分内核 — 首个独立版本

模块名：`fogkernel`

这是一个纯 Go 领域组件加本地诊断工作台的交付版本。场景、三版 Schema 工程和解析真值均由固定种子 `420261126` 在进程内确定性生成；不依赖 GPU、真实渲染管线、账号、Docker 或线上存储。

## 快速运行

```bash
go version                 # 已在 go1.22.12 linux/amd64 验证
GOPROXY=off go test ./...  # 使用本地模块缓存离线测试
go run ./cmd/fogctl verify
go run ./cmd/fogctl export --out artifacts/generated
go run ./cmd/fogctl serve --addr 127.0.0.1:18080
# 浏览器打开 http://127.0.0.1:18080
```

SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`，不需要 CGO 或系统 `sqlite3`。

## 实际验收结果

下列结果来自当前工作区实测；机器可读 JSON 保存在 `artifacts/verification.json`。

```text
$ go run ./cmd/fogctl verify
verification ok=true rays=20 hits=13 exactMaxTError=0.000e+00 previewMax=0.012928/0.050 diagnostic=true terminals=20/20
cross-version deepEqual=true maxTauDiffQ=0 maxTDiffQ=0
```

- 20 条确定性相机射线中 13 条命中介质，7 条为确定空结果；空结果为 `Hit=false`、`Transmittance=1`、`Blocks=[]`、`TerminalBlock=-1`。
- 精确路径对生成器解析真值的透射率、光学厚度和能量误差均为 `0`（同一解析公式与相同运算顺序；验收容差为 `1e-6` 透射率定点单位）。
- 预览路径射线级最大透射率误差约 `0.0129279`，逐块 `TOut` 最大误差约 `0.0380271`，均小于声明预览界 `0.05`。
- 诊断路径逐块复现精确路径，结论一致。
- 三个 Schema 装载后的规范场景 `reflect.DeepEqual=true`，跨版本逐块 τ/透射率定点差为 0。
- 三版本 × 三路径矩阵共 9 组，结论不随 Schema 改变。
- `go test -race ./...` 通过。
- `GOPROXY=off go test ./...` 与 `GOPROXY=off go build ./...` 通过，`go mod verify` 输出 `all modules verified`。
- 根包语句覆盖率实测约 `76.5%`（CLI 包不计入领域覆盖率）。

## 强制终止与信号实测

真实 `SIGKILL` 演练（动态临时 DB 路径，下面省略路径）：

```text
simulating forced termination after block commit 4 (ray 0 block 3)
forced_kill_exit=137
commit_state= ('scene-daea668ba02adbe2729488338041196b', 0)
ray_rows= [(0, 'in_progress', 4)]
block_count= 4

$ fogctl checkpoint --db <db> --resume
{
  "block_commits": 88,
  "committed_rays": 20,
  "status": "completed"
}
```

这验证了提交顺序：块与射线进度先落盘，全局射线高水位仍为 0；重启只把 `< committed_ray_count` 且状态为 `committed` 的射线视为完成，同时可复用第 0 条射线的连续块前缀。

可处理 `SIGTERM` 演练使用 `--block-delay-ms 20`，在运行中发送 `kill -TERM`：

```text
sigterm_exit=0
{
  "status": "signal_checkpoint",
  "code": "E_SIGNAL_SHUTDOWN",
  "committed_rays": 0,
  "block_commits": 8,
  "resume": "fogctl checkpoint --db <db> --resume"
}
```

检查点恢复后的结果与一次性全量求解逐射线、逐块比较一致；测试见 `checkpoint_test.go`。版本不匹配时 `OpenCheckpointStore`/`InspectCheckpoint` 返回 `E_CHECKPOINT_VERSION`，续算入口可自动清空旧检查点并整段重算；截断/损坏库映射为 `E_CHECKPOINT_TRUNCATED`。

## 需求到代码的映射

| 需求 | 主要文件 | 说明 |
| --- | --- | --- |
| 三版 Schema 装载 | `schema.go`, `schema_build.go`, `generator.go` | v1 PascalCase/扁平清单/‰/整数 mm/无 phase；v2 snake_case/体素树/‱/小数 m/显式 phase；v3 lowerCamel/体素树/‰/小数 m。所有版本映射到同一 `Scene`。 |
| 稳定业务错误 | `errors.go` | `E_SCHEMA_VERSION`、`E_DENSITY_SHAPE`、`E_FIXED_OVERFLOW`、`E_BACKPRESSURE`、`E_CHECKPOINT_*` 等。 |
| 分块积分 | `geometry.go`, `physics.go`, `integrate.go`, `truth.go` | AABB slab 求交、精确体素片段裁剪、按声明 chunk 分 bin、τ/T/能量累积、阈值与块数终止。 |
| 解析真值 | `generator.go`, `truth.go` | 固定种子生成双介质、四相机采样点、20 射线；真值记录每块 τ、T入/T出、能量、终止块和原因。 |
| 定点换算/审计 | `fixed.go`, `audit.go` | `int64` Q10^6 系数、Q10^6 µm 距离、Q10^9 τ；严格溢出错误与饱和转换；临界用例和 276 项跨版本逐块 τ 审计。 |
| 失效传播/增量 | `incremental.go` | 体素、系数、相机三类编辑；依赖图定位 direct/recomputed/propagated/reused closure；增量结果对全量重算做逐块检查。 |
| 预览/精确/诊断 | `integrate.go`, `verify.go` | 预览稀疏采样并插值重建；诊断保留体素片段；统一物理与换算逻辑。 |
| 并发预算/背压 | `integrate.go`, `requirements_test.go` | worker 数 1..64、每线程块缓冲 1..4096；有界 slot；`AdmitBlockBatch` 与 strict solver 返回 `E_BACKPRESSURE`；调度顺序/批次不改变数值。 |
| SQLite 续算 | `checkpoint.go` | 纯 Go SQLite；rays/blocks/commit_state 表；先块与射线状态、最后全局射线序号；版本匹配恢复。 |
| 工作台/注入 | `workbench.go`, `injection.go`, `web/index.html` | 零 CDN/零外部前端依赖的本地页面与 JSON API；12 类异常注入；源文档保持不可变。 |

## 稳定错误码

`E_SCHEMA_VERSION`, `E_SCHEMA_COMBINATION`, `E_SCHEMA_FIELD`, `E_DENSITY_SHAPE`, `E_DENSITY_RANGE`, `E_COEFF_NEGATIVE`, `E_FIXED_OVERFLOW`, `E_FIXED_SATURATED`, `E_FIXED_PRECISION`, `E_AUDIT_TOLERANCE`, `E_EDIT_TARGET`, `E_BUDGET_WORKERS`, `E_BUDGET_BUFFER`, `E_BACKPRESSURE`, `E_CHECKPOINT_OPEN`, `E_CHECKPOINT_WRITE`, `E_CHECKPOINT_TRUNCATED`, `E_CHECKPOINT_VERSION`, `E_CHECKPOINT_CONFLICT`, `E_INJECTION_UNKNOWN`, `E_SIGNAL_SHUTDOWN`。

## 工作台 HTTP API

- `GET /api/bootstrap`：场景、媒体、相机、配方、真值、验收报告、换算审计。
- `GET /api/project?schema=<schema-id>`：原始三版 JSON 工程。
- `POST /api/load`：装载指定 Schema，成功后替换工作副本；失败不替换当前会话。
- `POST /api/solve`：参数包含 `path`、`workers`、`buffer`、`order`、`batch_size`、`strict_backpressure`。
- `POST /api/compare`：跨版本 DeepEqual、版本/路径矩阵、276 项逐块 τ 审计。
- `POST /api/edit`：体素/系数/相机增量编辑，返回失效闭包、差异和全量一致性检查。
- `POST /api/reset`：恢复确定性工作副本。
- `POST /api/inject`：异常注入，错误体包含 `code`、`path`、`context`。
- `POST /api/checkpoint`：`crash`、`signal`、`resume`、`state` 四类对照。

## 范围说明

工作台以本机 HTTP + 浏览器页面形式交付“桌面诊断工作台”，避免 WSL/无显示环境下原生 GUI 与 Docker 依赖。所有前端资源通过 `go:embed` 编入二进制，不访问外网或 CDN。每条射线记录 `duration_ns`，该观测值只用于诊断，不参与正确性比较。
