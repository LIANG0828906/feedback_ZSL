# Fixed-Sampler：定点贴图采样与增量重算验证管线

一套从空目录交付的纯 Go、无 CGO、SQLite 持久化诊断管线。它使用固定种子生成贴图工程、
有序采样配置和解析真值，逐级执行定点重采样、色彩空间转换与位深量化，输出误差上界、
依赖图、精确失效清单、增量/全量比对和版本化检查点。

## 快速开始

要求 Go 1.22+。SQLite 驱动使用纯 Go 的 `modernc.org/sqlite`：

```bash
go mod tidy
bash scripts/validate.sh
```

也可以手动执行：

```bash
go run ./cmd/gen-samples -out samples -seed 20261125

go run ./cmd/sample-pipeline run \
  --project samples/blocks420/project.json \
  --db .validation/blocks.db \
  --out .validation/blocks-out \
  --workers 4 --buffer 2 --chunk-size 8

go run ./cmd/verify-result \
  --project samples/blocks420/project.json \
  --truth samples/blocks420/truth.json \
  --db .validation/blocks.db
```

Windows PowerShell：

```powershell
$env:CGO_ENABLED = "0"
go build ./...
powershell -ExecutionPolicy Bypass -File scripts\validate-windows.ps1
```

当前仓库已在 WSL Go 1.22.12 与便携版原生 Windows Go 1.22.12 上验证；详见
`docs/VALIDATION.md`。

## 生成的样本工程

固定种子 `20261125` 生成三组工程：

- `samples/blocks420`：48×32、4:2:0、两级 box 2× 降采样、12bit→8bit，包含色块边界。
- `samples/rampcolor444`：36×24、4:4:4、box 3×、BT.601 YCbCr→RGB 定点矩阵、10bit→8bit。
- `samples/mixed422`：64×32、4:2:2、nearest 2× 后 box 2×、12bit→10bit。

每个目录包含 `project.json` 与 `truth.json`。真值由生成器内独立整数参考和解析几何构造，
不读取采样引擎输出。

## 命令

### 运行管线

```bash
go run ./cmd/sample-pipeline run \
  --project <project.json> \
  --db <checkpoint.db> \
  --out <report-dir> \
  --workers <最大并行度> \
  --buffer <每 worker 采样任务缓冲上限> \
  --chunk-size <全分辨率坐标分块边长> \
  [--fresh] \
  [--crash-after-commit N]
```

当每线程缓冲已满，调度器记录
`per_worker_sampling_buffer_full_blocking_enqueue` 事件，然后阻塞排队；不会静默丢弃任务。

### 编辑配置与调整顺序

```bash
go run ./cmd/sample-pipeline edit \
  --in samples/blocks420/project.json \
  --out .validation/depth10.json \
  --seq 3 --bit-depth 10

go run ./cmd/sample-pipeline swap-order \
  --in samples/mixed422/project.json \
  --out .validation/reordered.json --a 1 --b 2
```

也可以直接编辑 JSON。缓存键包含配置序号和完整参数，因此交换顺序会使位置变化的级失效。

### 增量/全量比对

```bash
go run ./cmd/sample-pipeline run --project edited.json --db incremental.db ...
go run ./cmd/sample-pipeline run --project edited.json --db full.db --fresh ...
go run ./cmd/sample-pipeline compare --a incremental.db --b full.db --project-id blocks420
```

不一致时输出首个 `chunk_index`、`chunk_x/y`、`stage_seq` 与字段路径，并返回 `E_DIFF`。

### 独立验证工具

`cmd/verify-result` 不导入 `internal/engine` 或 `internal/checkpoint`。它只：

1. 从 SQLite 读取已持久化分块产物和版本/哈希元数据；
2. 从 `project.json` 读取有序配置；
3. 校验 `truth.json` 指纹；
4. 用 `internal/verifier` 自己的定点参考重算各级；
5. 逐分块、逐级、逐字段比对；
6. 检查已知采样点偏差和降采样边界坐标。

## 校验错误码

- `E_SCHEMA`：必填字段缺失、序号不连续、位深不在支持集合等。
- `E_DIM`：平面尺寸与色度采样声明不相容，或倍率不能整除当前平面；错误含 `plane` 与 `factor`。
- `E_KERNEL`：核类型未知或定点刻度越界；错误含 `config_seq`。
- `E_CONV`：转换意图在声明的源/目标空间之间不可达；错误含 `config_seq`、源空间和目标空间。
- `E_DIFF`：增量/全量或独立验证发现字段、分块、级或真值偏差不一致。

## 定点与误差报告

所有权重、矩阵、累加和量化均使用 `int64` 整数运算；代码路径没有浮点累加。

- 权重刻度：每级 `fixed_scale`，允许范围 `[256, 1<<30]`。
- 像素值刻度：`project.value_scale=1000000`，一个整数码值等于 `value_scale` 个定点单位。
- 每个输出像素记录截断分子、分母和余数；权重归一化也记录余数。
- `report.json` 给出四类统计和累积上界：
  - `resample_weight_normalization`
  - `downsample_accumulation_truncation`
  - `color_conversion_quantization`
  - `bit_depth_rounding`

每条上界都列出所依赖的观测值：`value_scale`、`fixed_scale`、`factor_x/y`、`tap_count`、
输入/输出位深、矩阵行 L1 和等。上界不使用真值反推。

## 依赖图与精确失效

缓存键由以下内容的规范 JSON SHA-256 得到：

- 算法版本与参数版本；
- 配置序号和完整 stage 参数；
- 分块坐标与分块尺寸；
- 实际核支撑区域的内容哈希；
- 与支撑区域相交的上游分块节点缓存键。

输出目录包含：

- `dependency_graph.json`：级、平面区域、覆盖分块、下游级和精确边；
- `invalidation.json`：分块编号、坐标、失效级、原因、旧/新缓存键；
- `report.json`：队列事件、提交顺序、残差和误差上界；
- `commit_sequence.json`：稳定分块提交顺序；
- `final_texture.json` 与 `levels/*.json`：最终和中间平面。

全局参数（核、倍率、矩阵、位深）通常影响该级所有非空分块；未与实际支撑区域相交的上游
分块不会进入缓存键。对齐样本中可验证依赖边保持 `from_chunk == to_chunk`，不向邻块扩散。

## SQLite 检查点与恢复

数据库包含：

- `chunk_checkpoints`：每个分块一行，原子保存各级产物、摘要、缓存键、内容/输入哈希、
  上游键、配置指纹、参数版本和算法版本；
- `node_cache`：内容寻址的级节点产物；
- `snapshots`：当前配置指纹的 `open/complete` 状态；
- `meta`：项目清单元数据。

`database/sql` 连接池限制为 1，且 `Store` 有互斥锁；每个分块的所有行在一个事务中提交，
因此写入串行且不会交错。重启时：

- 相同配置指纹的已提交分块可复用；
- 缺失分块按拓扑级重算；
- 任一行的内容哈希、参数版本或算法版本不匹配，会删除该项目的全部检查点和节点缓存并全量重算；
- 当元数据表明上次运行的配置指纹与当前相同时，行内配置指纹不一致也按快照损坏整体作废；
- 显式编辑配置后，旧指纹仅作为依赖规划输入；只有缓存键完全匹配的旧节点可复用，不会在同一
  新快照中混用未经验证的旧级结果。

`--crash-after-commit N` 在第 N 个分块事务提交后立即以 137 退出，用来复现强制终止边界。
用户 Ctrl-C 时，已提交分块保留；未提交事务由 SQLite 回滚。强杀后不承诺生成退出报告。

## 主要包

- `internal/model`：JSON 契约、错误码、分块产物和报告结构。
- `internal/samplegen`：固定种子样本与独立整数真值构造。
- `internal/loader`：schema、色度尺寸、倍率、位深、序号和转换可达性校验。
- `internal/engine`：定点计算、worker pool、依赖键、失效规划、调度、比对和误差报告。
- `internal/checkpoint`：纯 Go SQLite 事务与恢复/作废。
- `internal/verifier`：不依赖采样主路径的独立参考实现。
- `cmd/gen-samples`、`cmd/sample-pipeline`、`cmd/verify-result`：三个可执行入口。

## 验证结论

最近一次完整 `go test ./...`、`go vet ./...`、Linux/Windows 构建、三组样本独立验证、
增量/全量比对和崩溃恢复均通过。精确命令与摘要见 `docs/VALIDATION.md`。
