# DialogueFit 交付说明

本地、确定性的游戏对白韵律包络对齐与时长拨配组件。模块名为 `dialoguefit`，Go 版本 `1.22`，SQLite 使用纯 Go 驱动 `modernc.org/sqlite`；依赖已固定在 `go.mod` / `go.sum` 并放入 `vendor/`，验证时不需要网络或外部服务。

## 快速运行

```bash
go version
go test ./... -count=1
go test -race ./... -count=1

# 生成样本/真值，运行实时增量、全量、崩溃恢复三路径，写报告与 SQLite
go run ./cmd/dialoguefit demo --out artifacts/local-demo

# 独立复核工具（独立 cmd，不导入 envelope/fit/engine）
go run ./cmd/recheck \
  -db artifacts/local-demo/checkpoints.db \
  -truth artifacts/local-demo/final/truth.json

# 本地诊断页面
go run ./cmd/dialoguefit serve \
  --out artifacts/local-demo \
  --addr 127.0.0.1:8096
```

演示产物：

- `artifacts/local-demo/project.json`：固定种子生成的工程、PCM Base64、音素标注和参数。
- `artifacts/local-demo/truth.json`：基线构造真值。
- `artifacts/local-demo/final/truth.json`：四项编辑后的最终构造真值。
- `artifacts/local-demo/checkpoints.db`：最终片段级检查点。
- `artifacts/local-demo/resume.db`：崩溃恢复演练数据库。
- `artifacts/local-demo/report.json`：编辑失效清单、三路径比对、队列事件、提交顺序和独立复核结果。

## 组件结构

- `model/`：工程、PCM、音素、真值、结果、失效条目、错误码和确定性哈希。
- `projectio/`：JSON 装载、PCM Base64/PCM16LE 解码、结构校验和 `E_DECODE`。
- `generate/`：固定种子 `20261126` 的 6 条对白、60 个音素、514 帧样本与解析构造真值。
- `envelope/`：逐帧 RMS、居中平滑、全局归一化、元音重音定位和拉伸时间映射。
- `fit/`：静音保底、按响度权重降序的 weighted-waterfall 拉伸分配、上界截断与缺口再分配、`E_FIT`。
- `dag/`：节点依赖图、传递失效和拓扑排序。
- `engine/`：节点缓存、参数版本、精确失效清单、增量构建、三路径编排和恢复。
- `checkpoint/`：纯 Go SQLite 片段检查点；每片段一个串行写事务。
- `state/`：对白级 `pending -> running -> committed/failed` 状态机。
- `sched/`：最大并行度、单线程采样缓冲上限、总缓冲预算和待处理队列预算。
- `compare/`：增量/恢复/全量结果逐项比较，定位首个对白、音素和字段。
- `recheck/`：仅依据 SQLite 持久化片段与真值清单/指纹进行独立复核。
- `web/`：标准库 `net/http` 页面和 JSON API，无 CDN/前端外部依赖。
- `cmd/dialoguefit/`：`generate`、`demo`、`serve`、`recheck` 入口。
- `cmd/recheck/`：独立复核工具入口。

## 关键口径

### 包络

帧长/帧移均为 160 samples，平滑窗为 3。原始帧值为：

```text
RMS(frame) / 32768
```

随后做边界截断的三帧居中算术平均，并除以全对白最大平滑值完成归一化。原始帧计算可在线程池中分块并行，归约和归一化按帧序号确定执行，因此不受完成顺序影响。

### 拉伸

1. 先为静音段保留 `max(0, min_silence_samples - duration)`。
2. 剩余缺口按 `loudness_weight` 降序处理；权重相同按音素序号升序。
3. 每段整数上界为 `int(float64(duration) * (max_stretch - 1.0))`，与黄金样本的 Go/IEEE 截断口径一致。
4. 当前段吃到上界后，未满足缺口继续传给后续音素。
5. 每段记录 `weighted-waterfall-partial`、`weighted-waterfall-cap-hit`、`weighted-waterfall-no-gap`、`silence-preserved` 或 `silence-hold-minimum`。
6. 上界耗尽仍有缺口、目标时长短于源时长、静音保底超过缺口或系数非正，返回 `E_FIT`。

### 缓存键与失效

节点键由以下材料哈希得到：

```text
content_hash + phoneme_id + node_parameter_version + node_kind + upstream_keys
```

节点包括 `envelope`、`stress`、`stretch_coefficient`、`stretch_range`、`aligned_stress` 和 `phoneme_result`。区间节点显式依赖系数节点和前一区间节点，对齐节点依赖重音与拉伸区间。

编辑后先用确定性分配结果和旧结果做语义差异，只把实际变化的系数/区间、后续边界和对齐落点列入失效报告；未变化音素直接复用旧 `PhonemeResult` 与缓存键。

### SQLite 检查点

`segment_checkpoints` 主键为 `(dialogue_id, phoneme_index)`，每行包含：

- `commit_seq`：稳定的规范提交顺序；
- `content_hash`、`param_version`、`algorithm_version`、`truth_fingerprint`；
- `envelope_digest`；
- `stress_json`、`segment_json`；
- 完整 `PhonemeResult` JSON payload。

存储层互斥锁、`SetMaxOpenConns(1)` 和单片段事务保证写入不交错。重启时只要某对白缺少片段、顺序不完整，或任一行的内容/参数/算法/真值指纹不匹配，就删除该对白全部行并从第 0 个音素重算，不会拼接新旧片段。

## 实测结果

以下结果在当前目录、`go1.22.12 linux/amd64` 下实际执行：

- `go build ./...`：通过。
- `go vet ./...`：通过。
- `go test ./... -count=1`：全部包通过。
- `go test -race ./... -count=1`：全部包通过，无数据竞争。
- `go run ./cmd/dialoguefit demo --out artifacts/local-demo`：通过。
  - 对白：6；
  - 音素/分段：60；
  - 包络帧：514；
  - 重音：24；
  - 三路径：`full`、`incremental`、`resume` 均为 514 帧、24 重音、60 段、60 条提交序列；
  - `compare.RunsEqual`：增量/恢复与全量逐项相等；
  - 独立复核：`ok=true`，复核 6 对白、514 帧、24 重音、60 段、6 个指纹。
- 崩溃演练：在 `dlg-03` 提交第 3 个片段后注入 kill；重开 DB 检测到 `param-version`，整对白作废，恢复 5 条对白并重算 `dlg-03` 的 10 个音素。
- HTTP 实测：`/healthz`、`/`、`/api/state`、`/api/edit`、`/api/retry`、`/api/inject-failure` 均返回预期；编辑 `dlg-05` 时只报告 3 个实际变化音素。

四项规定编辑的失效计数与工作区已有参考报告完全一致：

| 编辑 | 对白 | 重算音素 | 复用音素 |
| --- | --- | ---: | ---: |
| `target-duration` | `dlg-01` | 7 | 3 |
| `stretch-cap` | `dlg-02` | 4 | 6 |
| `stress-tolerance=8` | `dlg-03` | 4 | 6 |
| `silence-minimum` + target | `dlg-04` | 6 | 4 |

`acceptance/golden_artifact_test.go` 还会在 `artifacts/demo/` 存在时读取该黄金样本，验证本实现的 514 个包络值、60 个拉伸区间、系数、上界标记和依据字段与其真值逐项一致。

## 诊断页面

页面左侧可选择对白并提交目标时长、拉伸上界、静音保底和重音容差；右侧展示：

- 源/拉伸后时间轴、音素类别、重音线和上界触发提示；
- 逐帧包络曲线与重音标注；
- 每段拉伸系数、上下界触发和分配依据；
- 本次/累计失效条目；
- 队列预算、排队原因和等待时长；
- 三路径计数与首个不一致字段；
- 独立复核计数和首个违规字段。

“注入失败”只设置当前演示引擎的对白级注入标记；之后提交任意编辑会使该对白进入 `failed`，其他对白和已提交结果不受影响。点击“重试对白”会从确定参数完整重建并串行提交该对白。
