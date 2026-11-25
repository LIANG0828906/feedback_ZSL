# OCRFlow 实现与验证说明

OCRFlow 是一个可集成的 Go 领域组件，模块名 `ocrflow`。它不依赖外部 OCR 服务：样本由固定种子确定性生成，识别使用本地版本化点阵模板，持久化和检查点使用 SQLite 驱动 `github.com/mattn/go-sqlite3` 与 Go 标准库。

## 快速运行

```bash
go version
go mod tidy
go test ./... -count=1

# 生成 PNG、真值和 v1/v2/v3 配方样本
go run ./cmd/ocrflow generate --out artifacts/samples

# 端到端验收：三版本、往返、增量/全量、恢复、异常码
go run ./cmd/ocrflow verify --work artifacts/demo

# 本地诊断页面
go run ./cmd/ocrflow serve --work artifacts/demo --addr 127.0.0.1:18080 --reset
# 浏览器打开 http://127.0.0.1:18080/
```

诊断接口：

- `/`：检测框叠加、逐行文本/置信度、失效传播、预算、检查点和缓存。
- `/api/state`：完整 JSON 状态。
- `/healthz`：进程与异常码说明。
- `/shots/{shot-id}/image.png`：带检测框的 PNG。
- `/action/invalidate-demo?shot=shot-0002&line=1`：注入区域替换并重跑。

## 领域结构

| 文件 | 职责 |
| --- | --- |
| `generator.go`, `font.go`, `sample_files.go` | 固定字模、渐变/几何背景、倾斜低对比文本、四点真值、样本落盘与清单校验 |
| `recipe.go`, `recipe_schema.go` | 内部配方模型、v1/v2/v3 DTO、字段路径校验、参数版本哈希 |
| `integer.go`, `geom.go`, `conversion_check.go` | 千分比/万分比、像素/分块坐标的纯整型换算、溢出码和往返残差 |
| `detect.go`, `imagecore.go` | 整型灰度、直方图归一化、梯度、笔画宽估计、连通候选、重叠/笔画一致性合并、掩膜 |
| `recognize.go` | 字符间距切分/序列切分、版本化模板匹配、空行标记、置信度和 CER |
| `graph.go`, `invalidate.go`, `region.go` | 依赖图、缓存键、配方字段失效、区域替换和失效清单 |
| `pipeline.go` | 检测/识别编排、缓存复用、分块提交、报告和增量/全量字段级比对 |
| `scheduler.go` | 最大并行度、每任务像素缓冲上限、队列数量/像素预算、阻塞等待记录 |
| `store_sqlite.go` | SQLite 缓存、单事务分块检查点、串行提交顺序、重启校验和整块作废 |
| `diagnostics.go` | 无外部 CDN 的本地 HTML/JSON/PNG 诊断界面 |
| `cmd/ocrflow/main.go` | `generate`、`verify`、`serve` 命令 |

## 需求落点

1. **确定性样本与 schema**：固定种子 `42` 生成 4 张 720×420 截图、共 8 行文本；包含单行和三行排行榜/结算文案。每行真值含有序四点、文本、rune 字符数和平均笔画宽（同时给像素整数与千分之一像素值）。三个 schema 分别使用 permille/BPS、字符串阶段/整数 `order_code`/对象阶段、块坐标/像素坐标、可选 `binarization` 字段等差异。错误为 `E_SCHEMA`，带 `source_version` 与完整字段路径。
2. **整型换算**：内部阈值统一为 BPS（1/10000），坐标统一为绝对像素。千分比乘 10；降刻度用 `nearest_half_up`；块坐标按角点 floor/ceil 保守包围。阈值往返残差声明为 `5 BPS`，8×8 分块坐标残差为 `7 px`，字符数必须相等。正整数乘法溢出返回 `E_OVF`，错误含 `field`、`operand_a`、`operand_b`。
3. **检测核心**：灰度系数 `299/587/114` 全整数；归一化使用直方图百分位和整数拉伸。候选掩膜为像素级 `Mask`，连通域输出笔画宽、梯度分和置信度，再按垂直重叠、高度比、水平间距和笔画宽一致性合并。相同输入重复检测及 8 并发检测的 JSON 字段完全一致；样本检测行数与真值相同，声明容差为 1。
4. **识别**：用笔画间距推断字符中心距和空格，过宽粘连域可走固定步长序列切分。模板库当前版本为 `tpl-1.0.0`，`V2TemplateLibrary()` 为 `tpl-1.1.0`；每个字符命中都带模板版本，匹配器只接收一个库，因此不会混版本。空结果输出 `empty=true` 的显式行；置信度以 `[0,10000]` BPS 存储并映射到 `[0,1]`。固定样本 CER 距离为 0。
5. **依赖图与键**：图中显式连接截图哈希、预处理、检测参数、检测结果、行候选、切分和识别。行键为：
   `SHA256(stage | region_hash | shotID:lineID | stage_param_version | upstream_key)`。
   识别字段编辑只失效 recognition；检测/预处理字段失效候选及其下游；区域替换只改变该行 `region_hash`。失效清单含截图、行号、原因、字段、阶段和旧键。
6. **增量一致性**：`CompareRunReports` 比较 shot/line 序列、包围框、文本、每字符字符值和置信度；首个差异返回 `E_DIFF`，带 `shot_id`、`line_no`、`diff_field`、期望值和实际值。区域替换实测 7 行缓存命中、1 行重算；其结果与空缓存全量重跑逐项相等。
7. **SQLite 检查点**：`CommitChunk` 在一个事务中写 chunk 头、按 ordinal 串行写所有行、UPSERT 该块缓存。单连接池 + 事务保证写入不交错，`commit_order` 单调稳定。重启时 `RecoverCurrentCheckpoints` 比对 shot hash、region hash、recipe/param/algorithm 版本；任一不匹配则删除该块及断链后继，返回应继续的前驱。未提交/强杀事务由 SQLite 回滚，不生成退出报告。
8. **预算、隔离和界面**：调度器限制 worker 数、单任务像素缓冲上限、最大排队任务和排队像素；预算满时阻塞提交并记录 `max_queued_tasks` 或 `max_queued_pixel_bytes` 与等待毫秒。截图截断注入返回 `E_DECODE`；重叠/笔画参数不在允许集合返回 `E_PARA` 和 `allowed_set`；单行故障可清除后重试，不影响其他行和已提交块。

## 实测结果

最终环境：

- `go version go1.22.12 linux/amd64`
- `CGO_ENABLED=1`, `CC=gcc`
- `github.com/mattn/go-sqlite3 v1.14.24`，CGO 编译通过（仅有上游 amalgamation 的 const 限定警告）
- 用户态 SQLite CLI：`sqlite3 3.46.1`（无 root 时从 `apt-get download sqlite3 libsqlite3-dev` 解包到 `~/.local/bin`/`/tmp/sqlite-local`；Go 驱动自带 SQLite C 源码，不依赖运行时头文件）

已执行：

```text
go test ./... -count=1
ok  ocrflow  (...）

go test -race ./... -count=1
ok  ocrflow  10.571s
```

`go run ./cmd/ocrflow verify --work artifacts/demo` 的 9 项检查全部 PASS：

```text
deterministic_samples       shots=4 files=8 recipes=3
three_schema_recipes        loaded v1/v2/v3 with field-path validation
integer_roundtrip           items=2 threshold_tol=5BPS coord_tol=7px
detect_and_recognize        lines=8 CER_distance=0 commits=6
precise_region_invalidation hits=7 misses=1 expected_hits=7
incremental_full_diff       text, char confidence, boxes and line sequence equal
recipe_edit_incremental_diff invalidated=8 lines and full/incremental equal
healthy_recovery_scan       valid_chunks=6
forced_termination_recovery discarded all chained chunks after tampered prefix
```

诊断服务实测：

```text
GET /healthz                 200
GET /api/state               200
GET /                        200，包含检测框叠加/失效传播/预算占用/全部异常码
GET /shots/shot-0002/image.png 200 image/png 720x420
SQLite checkpoint_chunks=6, checkpoint_lines=8
cache stages: line_candidate=8, segmentation=8, recognition=8
```
