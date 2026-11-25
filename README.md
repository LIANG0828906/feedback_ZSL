# 亮度蒙版边缘细化管线与验证工具

一个仅使用 Go 标准库与纯 Go SQLite 驱动 `modernc.org/sqlite` 的确定性、可中断、可恢复的交互式亮度蒙版边缘细化样例系统。它不依赖外部站点、账号、Docker、CGO 或前端 CDN，可在 Windows 上直接运行。

## 快速开始（Windows PowerShell）

要求 Go 1.22+：

```powershell
go version
go env GOPATH GOMODCACHE CGO_ENABLED GOOS
go mod tidy

# 生成确定性样本、真值清单和预算参数
go run ./cmd/samplegen -dir samples

# 环境自检
go build ./...
go test ./...

# 一次性精确运行并写 SQLite 检查点
go run ./cmd/edgepipe run --sample step128 --db checkpoints/edge.db

# 独立验证工具：不导入主管线，只凭检查点产物和真值指纹重算
go run ./cmd/verify --sample-dir samples/step128 --db checkpoints/edge.db

# 实时预览 / 断点恢复 / 一次性全量重算三路径比对
go run ./cmd/edgepipe three-path --sample step128

# 启动本地诊断页面
go run . --addr :8080
# 或：
go run ./cmd/edgepipe serve --addr :8080 --samples samples --db checkpoints/edge.db
Start-Process http://localhost:8080
```

Linux/macOS 将 `Start-Process` 换成 `xdg-open`/`open` 即可。也可以交叉编译 Windows 二进制：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

## 确定性样本

`cmd/samplegen` 固定种子 `42026801/2/3`，生成：

- `step128`：128×96，4×3 个 32px 分块。
- `gradient160`：160×128，5×4 个分块。
- `spots96`：96×96，3×3 个分块。

每张图包含明暗台阶、线性渐变带、三角形斜边、嵌入的孤立亮点/暗斑、确定性椒盐点和高斯噪声。`truth.json` 记录：

- 每条已知边的编号、类型、起止坐标和亚像素参考线。
- 解析覆盖率生成的逐像素参考蒙版 `reference_mask.png` 及其 SHA-256。
- 图像分辨率、通道数、位深和真值指纹。
- 分块尺寸 `32`、细化迭代预算 `5`、结构元素半径 `1`、预览延迟上限 `100ms`。
- 收敛容差 `0.85px`、边缘中位容差 `0.75px`、最大上界 `4px`；P95 作为诊断字段输出，当前样本实测低于 `0.6px`。

加载样本时会校验 PNG 可解码、分辨率/通道/位深、真值指纹、参考蒙版哈希、分块整除和参数范围。

## 处理管线

1. **亮度分位阈值**：每块建立 256 桶直方图，用确定性 nearest-rank 分位估计背景电平和前景电平，阈值取二者中点。类间间隔小于 48 个亮度级的同质块进入保护分支，避免把纯噪声劈成两类。
2. **前景/背景分离**：同一阈值快照驱动预览和精确重算，保证两条交互路径使用完全相同的分位参数与阈值。
3. **形态学边缘追踪**：用圆盘结构元素执行开运算、闭运算和内部边界骨架提取；形态学阶段拼接相邻块前景并按半径填充，避免块边界钳制产生假边。
4. **迭代亚像素细化**：为每个二值跳变点建立阈值交叉位置，按固定阻尼 `position += 0.5(target-position)` 同步迭代，再把位置栅格化为软覆盖率蒙版。迭代记录最大/平均残差和变化像素数。
5. **全局边缘定位**：拼接所有块的软蒙版后做跨块亚像素插值；边缘点按方向过滤并唯一分配给最近真值边，输出每条边和全图的中位数、P95、最大值和有符号中位偏差。

## 状态机与提交边界

每个分块有三个阶段：

- `foreground_separation`
- `edge_tracing`
- `refinement`

每个阶段有四种状态：`pending`、`processing`、`committed`、`failed`。合法迁移为：

- `pending -> processing`
- `processing -> committed | failed | pending`
- `failed -> processing`

非法迁移返回 `E_STATE` 并保持原状态。进度为已提交阶段数除以 `块数 × 3`。已提交阶段的阈值、半径和细化结果从 SQLite 恢复并冻结；修改参数只会作用于缺失/未提交阶段。

调度器可使用多个 worker 并发计算，但按“阶段 → 行优先分块”的规范顺序提交，因此提交序列不随 worker 数量或完成顺序变化。

## 预览与三路径语义

拖动半径/阈值时：

- 迭代预算从 `5` 降到 `2`。
- 结构元素步进为 `2`，半径按步进粗化。
- 返回粗预览蒙版和偏差占位，记录实际预算、步进、延迟和判定依据。

松开控件后，使用最后一次预览的同一个 `ThresholdSnapshot` 全精度重算，并比较：

- 边缘偏差中位数差。
- 收敛块比例差。
- 逐像素不同比例。

任一项超过 `truth.json.params` 中声明的容差即返回 `E_DIVERGE`，错误详情包含三项实测差异、容差、首个不一致块和块内/全局差异像素。

粗预览本身是被允许近似的路径；“三路径逐像素一致”指三个权威最终路径：

1. 一次性全量精确重算。
2. 写入部分检查点、关闭并重新打开 SQLite 后的断点恢复结果。
3. 交互粗预览停止后的全精度提交结果。

三者的全局蒙版哈希、偏差哈希、精度报告哈希和阶段提交序列完全一致。

## SQLite 检查点

数据库包含版本化 `schema_meta` 和 `stage_checkpoints` 表。每行保存：

- 样本、块坐标、阶段和状态。
- 输出 JSON、摘要和收敛记录。
- `content_hash = SHA256(stage-output-v1 + output_json)`。
- 参数版本、迭代预算、算法版本和真值指纹。
- 自增提交序号。

`Store` 使用 `MaxOpenConns(1)`、互斥锁和事务，保证同进程写入串行且原子。恢复时逐行重算哈希并核对算法版本/真值指纹；任一不匹配会删除该样本全部检查点并全量重算。

## 独立验证工具

`cmd/verify` 与 `internal/verify` 不导入 `internal/pipeline` 或 `internal/runner`。它从 SQLite 读取阶段 DTO 和灰度快照，用包内自有的形态学、亚像素细化和几何比对代码重新计算：

- 前景 → 清理/骨架是否一致。
- 细化蒙版和逐像素残差是否一致。
- 全局边缘偏差是否满足真值容差。

发现不一致时输出首个违规块坐标、阶段标识和字段名。

## 诊断页面与 HTTP API

页面地址：`http://localhost:8080/`

页面展示原图、前景分离、清理掩码、骨架、细化掩码、边缘叠加、残差热力图、预览/精确差异热力图、状态矩阵、收敛比例、精度和三路径结果；支持阈值/半径拖动预览、停止后精确重算、暂停、续跑、输入块坐标重试和 ZIP 导出。

主要 API：

- `GET /api/samples`
- `POST/GET /api/run?sample=step128&workers=4`
- `GET /api/preview?sample=step128&radius=2`
- `GET /api/finalize?sample=step128&radius=1`
- `GET /api/state?sample=step128`
- `GET /api/pause?sample=...` / `/api/resume?sample=...`
- `GET /api/retry?sample=...&tx=0&ty=0`
- `GET /api/verify?sample=...`
- `GET /api/three-path?sample=...`
- `GET /api/radius-sweep?sample=...`
- `GET /api/export?sample=...`
- `GET /viz?sample=...&kind=original|separation|clean|skeleton|refined|overlay|heatmap|diff`

## 错误码

| Code | 触发场景 | 关键字段 |
| --- | --- | --- |
| `E_DECODE` | PNG/真值/参考蒙版损坏 | path |
| `E_PARA` | 分位、位深、尺寸、预算、半径等越界 | `field`、`legal_range` |
| `E_ITER` | 迭代预算耗尽且残差超过容差 | tile、residual |
| `E_EDGE` | 边缘中位/最大偏差超差 | edge id、observed/truth 坐标 |
| `E_STATE` | 非法状态迁移 | tile、stage、from/to |
| `E_DIVERGE` | 预览与精确结果发散 | 三项实测值、first tile、diff pixel |
| `E_CHECKPOINT` | 检查点哈希或写入冲突 | field/hash |

## 主要目录

- `cmd/samplegen`：样本生成器入口。
- `cmd/edgepipe`：生成、运行、三路径、HTTP 服务 CLI。
- `cmd/verify`：独立验证工具。
- `internal/model`：领域模型、默认声明参数和参数校验。
- `internal/sample`：确定性几何样本生成与加载校验。
- `internal/quantile`：确定性直方图分位。
- `internal/morph`：二值形态学原语。
- `internal/pipeline`：阈值、边缘追踪和迭代细化主路径。
- `internal/edgeeval`：真值边几何统计。
- `internal/state`：三阶段四态状态机。
- `internal/store`：版本化 SQLite 检查点。
- `internal/runner`：并发调度、提交顺序、预览、恢复和三路径比较。
- `internal/verify`：独立重算验证器。
- `internal/viz`：PNG/热力图/叠加渲染。
- `internal/web`：`net/http` API 和内嵌诊断页面。

实际验证输出见 `VALIDATION.md`。
