# IconCut 掩码分割组件交付说明

本交付是一个可验收的 Go 组件，覆盖确定性样本生成、几何校正、校正后分块、区域生长与形态学、预览/精确双路径、SQLite 版本检查点、受限并行调度、三路径比对、独立验证器、异常注入恢复和本地/Wails 界面。

## 代码地图

| 需求 | 实现位置 |
| --- | --- |
| 错误码、清单/真值模型 | `errors.go`, `model.go` |
| 固定种子多样本生成、PNG/真值输出 | `sample.go`, `pngcodec.go`, `bundle.go` |
| 单应、径向畸变、定点反向映射、0/1/2 阶重采样 | `geom.go` |
| 校正后分块、源多边形/包围盒、重叠边带、`E_BAND` | `tile.go` |
| 参数校验、区域生长、形态学、连通域/孔洞、真值比对 | `segment.go` |
| 预览降采样、迭代预算、`E_DIVERGE` | `preview.go` |
| 四阶段状态机、SQLite 快照/提交/恢复/哈希校验 | `checkpoint.go` |
| 预算线程池、队列等待、暂停/取消 | `scheduler.go` |
| 四阶段管线、故障注入、逐块重试 | `pipeline.go`, `actions.go`, `artifact.go` |
| 三路径掩码/统计/提交序列比对定位 | `compare.go` |
| 独立 SQLite 验证工具 | `verifier/verifier.go`, `cmd/verifycut` |
| CLI | `cmd/iconcut/main.go` |
| 本地 Web 验收台 API 与静态界面 | `ui/server.go`, `ui/static/` |
| Wails 壳 | `cmd/wailsui/main.go`（Windows 标签）、`wailsapp/` |
| 一键自检 | `scripts/acceptance.sh`, `scripts/acceptance.ps1` |
| 自动化验收用例 | `acceptance_test.go`, `verifier/verifier_test.go` |

## 核心设计

### 1. 确定性样本与装载校验

`DefaultSpecs` 生成三张不同分辨率样本：`icon-144`、`icon-192`、`icon-256`。每张图包含：

- 四个色块区域，其中 `amber_plate` 带一个孔洞；
- 近色渐变背景、低对比度目标和确定性哈希噪声；
- `manifest.json`：几何参数、控制点、区域真值、分块、预算和容差；
- `source.png`：经透视/径向几何映射产出的畸变源图；
- `reference.png`：像素级标签真值，像素值为区域 ID `1..4`；
- `corrected_ideal.png`：确定性校正理想图；
- `truth.sha256`：真值指纹。

装载时校验图像尺寸/通道、有限数值、单应行列式、径向半径、控制点、分块整除关系和参数区间。图像损坏返回 `E_DECODE`；清单字段不符返回 `E_META`，错误对象含 `field` 路径与 `details`。

### 2. 几何校正

变换方向为“校正后坐标 → 无畸变单应坐标 → 径向畸变源坐标”：

1. 对校正后像素应用声明的 `3×3` 单应；
2. 以畸变中心、半径和 `k1/k2` 应用径向模型；
3. 坐标按 `coord_scale=16` 量化为定点 `SXQ/SYQ`；
4. 每个 `MapEntry` 保存量化残差；
5. 支持最近邻、双线性和双三次采样。

`ControlResiduals` 给出控制点几何/有效残差统计；测试断言所有控制点小于声明的 `1px` 容差，且 0/1/2 阶残差统计单调不增。同一输入和参数两次校正的 RGB 字节完全一致。

### 3. 校正后分块与边界无关性

分块矩形在校正后画布生成，索引顺序为 `ty` 外层、`tx` 内层。每块保存：

- `core`：最终拥有的像素；
- `band`：按声明边带扩展的校正坐标；
- `polygon`：边带四角反向映射后的源多边形；
- `source_rect`：边带映射包围盒加插值核支撑；
- `required_band`：随插值阶数联动的保守边带宽度。

`CorrectTile` 使用源局部缓冲和同一套定点采样规则。测试逐像素比较所有块核心与 `CorrectWhole`，并额外验证 `24×24` 任意分块方案。边带为 0 时返回 `E_BAND`，详情含 `tile_index/tx/ty/band_px/required_band`。最终掩码按核心坐标拼回，块执行顺序不影响结果。

### 4. 分割与统计

`RegionGrow` 使用：

- 种子点颜色作为参考色；
- RGB 欧氏颜色距离和声明容差；
- Sobel/邻域梯度屏障；
- 确定性四邻接队列顺序；
- 背景种子检测和最小生成面积检查。

随后执行 3×3 方形结构元的先开后闭，并对大孔洞做拓扑保护，避免 2px 薄孔壁被开运算误删。`AnalyzeLabels` 计算面积、质心、外接框和四邻接背景连通孔洞数。与真值不一致时 `CompareToTruth` 返回 `E_REGION`，列出 `extra`、`missing`、区域指标和孔洞偏差。

### 5. 预览与精确重算

拖动参数时调用 `RunPreview`：

- 默认 `downsample_step=2`，分辨率比例 `0.5`；
- 迭代预算为 `max(128, smallW*smallH/2)`；
- 记录实际降采样步长、迭代预算、实际迭代数、延迟和判定依据；
- 预览统计按比例放回全分辨率坐标；
- 预算被截断时不执行额外边界恢复，保证迭代预算真实有效。

停止交互后，`RunBundle` 使用同一份 `SegmentParams` 计算参数版本并全精度重算。精确路径不读取 `iterations_used`，因此不会把预览实际步数带入最终结果。`ComparePreview` 比较区域数量、质心最大偏差和面积最大相对误差；超差返回 `E_DIVERGE`，详情含三项实测值、首个不一致区域和首个差异像素坐标。

### 6. SQLite 检查点与状态机

`checkpoint.go` 使用纯 Go 的 `modernc.org/sqlite v1.34.5`，表结构与随附 `artifacts/acceptance/icon-192/checkpoint.db` 对齐：

- `meta`
- `snapshots`
- `assets`
- `commits`
- `tile_state`

每块的 `mapping/grow/clean/stats` 四阶段均有 `pending/running/committed/failed` 状态。非法迁移返回 `E_STATE` 并保留原状态和进度。

每次提交在同一事务中写入 `commits` 和 `tile_state=committed`，保存：

- `summary_json`
- gob 编码的阶段 `artifact`
- `digest` 与 `content_hash`
- `mapping_version/scheme_version/param_version`
- `algorithm_version`

写连接设置为 `MaxOpenConns(1)`，并通过互斥锁串行化。正常路径按阶段波次、块索引顺序提交，因此 `commit_seq` 稳定。重启时将遗留 `running` 恢复为 `pending`；若任一哈希或版本不匹配，`ValidateCheckpoint` 返回损坏结论，`RunBundle` 清空该镜像旧快照并全量重算，禁止混用阶段产物。

### 7. 受限并行与三条路径

`Scheduler` 强制：

- `workers` 最大并行度；
- `max_queued_tasks` 队列预算；
- `max_thread_pixels` 单任务像素缓冲上限。

队列满时提交方阻塞，记录 `QueueWait{reason:"queue_budget_exhausted", wait_ns, wait_ms, tile, stage}`；像素预算超限返回 `E_PARA`。

测试覆盖：

- 一次性全量运行；
- 单 worker/单队列的实时增量式调度；
- 提交 7 条后中断、重开数据库恢复；
- 三者拼接掩码字节一致、区域统计一致、逻辑提交序列和内容哈希一致。

`CompareLabelMasks/CompareStats/CompareCommitSequences` 会定位首个差异的块坐标、阶段和字段。

### 8. 独立验证、异常隔离和界面

`verifier.VerifyDB` 只读取 SQLite：

- 从 `meta.snapshot_json` 取参数、清单、分块和版本；
- 从 `assets.source.rgb` 取源图像；
- 校验每条提交哈希和版本；
- 用验证器自己的定点映射和双线性实现重算每个 mapping 块的 RGB；
- 拼接 clean 标签；
- 用独立聚合/孔洞洪泛重算区域统计；
- 输出首个违规的 `tile_index/tx/ty/stage/field`。

单块故障由 `FaultController` 注入，任务捕获后将该块该阶段置为 `failed`，其他块和已提交阶段继续保留。再次运行时失败块回到 `pending/running` 并重试；测试证明重试后的掩码和统计与无故障路径一致。

异常链路：

- 空校正画布：`E_EMPTY`
- 种子在背景：`E_SEED`
- 注入生长越界：`E_GROW`
- 边带不足：`E_BAND`
- 参数越界：`E_PARA`
- 真值区域/孔洞不符：`E_REGION`
- 预览分歧：`E_DIVERGE`
- 非法状态迁移：`E_STATE`

界面：

- 浏览器验收台：`go run ./cmd/iconcut serve -addr 127.0.0.1:8787`
- Wails Windows 壳：`cmd/wailsui`（根模块，`windows` 构建标签）
- 独立 Wails CLI 工程：`wailsapp/`

界面展示源图、校正图、掩码、真值、预览参数/延迟/分歧、块阶段矩阵、三路径比对、独立验证结果，并提供暂停/恢复/取消和失败块重试按钮。

## 常用命令

```bash
# 格式化、vet、全部测试、构建、样本运行和独立验证
./scripts/acceptance.sh

# 或者逐步执行
gofmt -w .
go vet ./...
go test -count=1 ./...
go build ./...

go run ./cmd/iconcut generate -out samples
go run ./cmd/iconcut run \
  -bundle samples/icon-192 \
  -db checkpoints/icon-192.db \
  -out out/icon-192
go run ./cmd/verifycut -db checkpoints/icon-192.db
go run ./cmd/iconcut serve -addr 127.0.0.1:8787 -workspace .ui-workspace
```

Windows PowerShell：

```powershell
.\scripts\acceptance.ps1
```

Wails：

```powershell
cd wailsapp
npm install
wails build -s -skipbindings
# 或在仓库根交叉检查 Windows 壳：
$env:GOOS="windows"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build ./cmd/wailsui
```

SQLite 人工检查：

```bash
sqlite3 checkpoints/icon-192.db '.tables'
sqlite3 checkpoints/icon-192.db 'select tile_index,stage,state,attempt,error_code from tile_state order by tile_index,stage;'
sqlite3 checkpoints/icon-192.db 'select commit_seq,tile_index,stage,content_hash from commits order by commit_seq;'
```

## 实测结果

实测环境：Go `go1.22.12 linux/amd64`，Node `v20.19.5`，npm `10.8.2`，Wails CLI `v2.10.2`。当前容器未安装 `sqlite3` CLI；数据库由 `modernc.org/sqlite` 和 Python `sqlite3` 可读，表结构按人工 CLI 兼容设计。

以下命令在交付目录实际执行通过：

```text
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build ./...   # exit 0
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go vet ./...     # exit 0
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -count=1 ./...
    ok  iconcut           0.733s
    ok  iconcut/verifier  0.099s
```

详细测试：

```text
--- PASS: TestGeneratorLoadValidationAndMetaCodes
--- PASS: TestGeometryControlsMonotonicAndReproducible
--- PASS: TestTilesExactWholeAndBandError
--- PASS: TestParameterSeedGrowEmptyCodes
--- PASS: TestRegionMismatchListsExtraAndMissing
--- PASS: TestStateMachineIllegalTransitionKeepsState
--- PASS: TestPipelinePassesTruthAndThreePathsIdentical
--- PASS: TestCheckpointCorruptionInvalidatesSnapshot
--- PASS: TestFaultRetryProducesSameMask
--- PASS: TestSchedulerQueueWaitAndBufferCap
--- PASS: TestPreviewWithinToleranceAndDivergenceCode
ok      iconcut
--- PASS: TestVerifiesGeneratedCheckpoint
ok      iconcut/verifier
```

`./scripts/acceptance.sh` 最终输出 `ACCEPTANCE_OK`。本地生成的 `icon-192` 运行结果：

```text
commits=48 regions=4
1 area=2962 centroid=(46.187,48.664) holes=1
2 area=2332 centroid=(142.541,42.075) holes=0
3 area=1925 centroid=(97.084,109.108) holes=0
4 area=1015 centroid=(36.000,110.000) holes=0
verifier: ok=true commits_checked=48 tiles_checked=12 regions_computed=4
```

随仓库已有 `artifacts/acceptance/icon-192` 兼容样本也实际运行并通过独立验证：

```text
1 area=2966 centroid=(46.216,48.678) holes=1
2 area=2340 centroid=(142.500,42.000) holes=0
3 area=1909 centroid=(97.047,109.189) holes=0
4 area=1015 centroid=(36.000,110.000) holes=0
verifier: ok=true commits_checked=48 tiles_checked=12 regions_computed=4
```

UI 冒烟测试通过：

```text
GET  /api/samples   -> 3 samples
POST /api/run       -> ok, 48 commits, 4 regions
GET  /api/preview   -> step=2 budget=3456 within_delay=true divergence=null
GET  /api/verify    -> ok, 48 commits
GET  /              -> HTTP 200
```

Windows 交叉编译也通过：

```text
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...       # exit 0
cd wailsapp && GOOS=windows GOARCH=amd64 go build .          # 13M PE32+ iconcut-wails.exe
```

## 说明与边界

- 样本和真值全部由程序确定性生成，不访问外部图片、账号、付费服务或 GPU。
- 几何/分割算法选择了可验证、可复现的经典实现，而不是不可解释的模型。
- 预览路径刻意保留降采样和迭代上限；它负责快速占位，最终结论始终由全精度重算产出。
- 三路径一致性指“一次性全量、实时增量/小队列调度、中断恢复”三条最终精确路径；预览是低精度交互路径，通过区域容差和 `E_DIVERGE` 负责偏差检测。
