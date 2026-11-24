# 游戏录屏素材完整性核验内核

本交付是一个无外部服务、纯 Go SQLite、可离线复现的核验内核。代码不依赖 Docker；样本由固定种子在本地生成，所有报告和数据库均落在工作区。

## 快速运行

Linux/macOS：

```bash
bash scripts/reproduce.sh
```

Windows PowerShell：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\reproduce.ps1
```

脚本会设置 `CGO_ENABLED=0` 与 `GOPROXY=off`。Windows 检查点数据库默认写入：

```powershell
$env:LOCALAPPDATA\RecVerify\noise.db
```

也可以通过 `-db` 指定任意本地持久化路径。SQLite 驱动为 `modernc.org/sqlite`，是纯 Go 实现，交叉编译检查已通过：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
```

## 常用命令

```bash
go run ./cmd/gensample -out data/sample

go run ./cmd/verify run \
  -sample data/sample/noise \
  -db artifacts/noise.db \
  -report artifacts/report-noise.json \
  -parallelism 4 \
  -buffer-pixels-per-thread 24576 \
  -block-size 16 \
  -window 3 \
  -threshold 0.55 \
  -inject-failure-once 8:noise_variance

go run ./cmd/verify experiment \
  -data data/sample \
  -out artifacts/experiment-report.json \
  -db-dir artifacts/experiment-dbs \
  -parallelism 4 \
  -cancel-after 2

go run ./cmd/review \
  -db artifacts/noise.db \
  -truth data/sample/noise/truth.json \
  -out artifacts/review-noise.json
```

`verify run` 支持 `-parallelism`、`-buffer-pixels-per-thread`、`-block-size`、`-window`、`-threshold`、`-watermark-threshold`、`-batch-frames`。

## 确定性样本

生成器为每段输出：

- `frames/frame_NNN.rmf`：带 `RCVFRM01` 头的确定性 RGB 帧。
- `baseline/frame_NNN.rmf`：未替换的原始基准像素。
- `truth.json`：时间戳、内容/基准哈希、每帧水印区域、图元外接框、替换区间掩码和真值指纹。

内置三段：

| 片段 | 替换帧（1-based） | 注入内容 | 预期定位 |
| --- | ---: | --- | --- |
| `noise` | 8–10 | 36×28 图元区域替换 + 高频噪声 | 8–11，末端 11 为恢复边界，真值容差 1 |
| `watermark` | 7–9 | 缺失、错号、错位 | 7–9 |
| `clean` | 无 | 无替换 | 无区间、无误报 |

## 异常码

| 码 | 触发场景 | 定位字段 |
| --- | --- | --- |
| `E_DECODE` | 清单缺失、JSON/帧头/像素截断、哈希不符 | `truth`、`frame_data`、`content_sha256` |
| `E_META` | 宽高、通道、帧数或清单长度不符 | `width`、`height`、`channels`、`frame_count` |
| `E_ORDER` | 时间戳倒置 | `timestamp_ns` + 首个违规帧号 |
| `E_PARA` | 格点、阈值、分块、窗口、缓冲上限越界 | `grid_x`、`block_size`、`residual_window` 等 |
| `E_RANGE` | 声明或修正后的水印区域越界 | `watermark_region` + 帧号 |
| `E_DIFF` | 增量/全量或独立复核不一致 | 帧号、块坐标、证据类型、字段名 |
| `E_STORE` / `E_FAULT` | SQLite 或可重试证据计算故障 | 错误详情/证据节点 |

## 证据 DAG 与缓存键

显式依赖图由 `verify.DependencyGraph()` 输出，并写入报告的 `dependency_graph`：

```text
watermark_decode -> timeline_continuity
source frames    -> residual_energy
source frame     -> noise_variance
truth bboxes     -> edge_consistency
watermark + timeline + residual + noise + edge -> decision
```

缓存键为：

```text
SHA256(algorithm_version + content_hashes + evidence_type
       + relevant_parameter_values + upstream_cache_keys)
```

每类证据只注入真正影响它的参数子集。例如：

- `block_size`：只使 `noise_variance` 直接失效，再传播到 `decision`。
- `tamper_threshold`：只使 `decision` 失效。
- `watermark_region`：使 `watermark_decode` 失效，再传播到 `timeline_continuity` 和 `decision`。
- `residual_window`：使 `residual_energy` 失效，再传播到 `decision`。

参数实验会先从 SQLite 重开并 `Recover`，然后应用变更；未失效节点的键仍命中持久化证据。失效清单包含帧号、证据类型、旧缓存键和 `param:<field>` / `upstream:<type>` 原因。

## 三路径一致性

`experiment.RunThreeWay` 对同一样本、参数、并行度和批大小运行：

1. `realtime`：空 SQLite，按帧批实时提交。
2. `resume`：提交前两个检查点后停止，重开数据库，严格恢复并继续。
3. `full`：另一个空 SQLite，一次性跑完所有帧批。

比较内容包括：

- 逐帧 `DecisionEvidence` 全字段；
- 逐帧 `NoiseEvidence.Blocks`，差异可定位到帧、`block_x/block_y` 和 JSON 字段；
- 替换区间；
- 最终规范提交序列（帧、证据拓扑、缓存键）；
- 实际 SQLite `seq, ordinal` 提交序列。

不一致返回 `E_DIFF` 和首个 `Difference`。

## SQLite 版本化检查点

表：

- `run_meta`：真值指纹、内容根、算法版本、参数计划哈希。
- `checkpoints`：稳定自增 `seq`、批 ID、参数版本、算法版本、真值指纹、内容根、证据摘要、重算/命中计数。
- `evidence`：当前证据缓存键、上游键 JSON、载荷 JSON、载荷 SHA256、`ok/failed` 状态。
- `batch_items`：每个检查点内稳定序号。

`Store.SubmitBatch` 在单个事务中写检查点、证据和批条目；`sync.Mutex` 与 `SetMaxOpenConns(1)` 保证共享写入串行、批序稳定。

恢复时若真值指纹、内容根、算法版本或参数计划不匹配，会清空 `batch_items/evidence/checkpoints/run_meta`，禁止部分混用。用户在批之间取消时，只有已事务提交的批可恢复；强制终止时未提交事务由 SQLite 回滚，工具不会把半截批当作完整报告。

## 并发与资源

- `workerPool` 使用容量为 `-parallelism` 的信号量限制证据计算并发。
- `BufferLimiter` 总容量为 `parallelism * buffer_pixels_per_thread`。
- 单次像素申请超过线程上限会得到 `E_PARA`/资源错误；容量不足时在条件变量上排队，并记录帧号、证据类型、像素数、原因和等待毫秒。
- 单节点故障写为 `status=failed`，不会进入检查点批；其他节点和此前已提交批不受影响。重试成功后以同一缓存键替换为 `ok`，最终快照与无故障路径逐项一致。

## 独立复核边界

`cmd/review` 只读取：

- SQLite 中的持久化证据与 `run_meta.truth_fingerprint`；
- 真值 JSON（先重算并核对指纹）。

`internal/review` 不导入 `internal/verify` 主核验路径。它独立校验载荷摘要、置信度公式、噪声块坐标/计数、中位数/MAD/离散度，并从 `decision.tampered` 重新扫描连续区间。发现问题时输出首个：

```json
{
  "code": "E_DIFF",
  "frame": 8,
  "evidence_type": "noise_variance",
  "item": "median_variance"
}
```

## 算法说明

- 水印：在声明矩形内按 `grid_x × grid_y` 取格点中心亮度，前 32 个格点按大端位编码帧号；输出每位 bit、亮度和置信度。
- 时间轴：校验解码值等于帧号、相邻可靠解码值满足 `+1`，并汇总缺失、低置信/错位和跳号。
- 残差：计算当前帧与前一帧亮度 MSE，并相对前 `window` 个跳变能量计算稳健 Z 分数。
- 噪声：每块计算 Laplacian 高频响应，用中位数/MAD 得到鲁棒方差，再跨块计算 Z 分数、离散度和离群块比例。
- 图元一致性：在真值外接框内计算 Sobel 方向直方图，与前一帧做直方图交。
- 最终置信度：`clamp01((score - tamper_threshold) / (1 - tamper_threshold))`；阈值越高（定罪越严格），同一证据的置信度单调不增。
