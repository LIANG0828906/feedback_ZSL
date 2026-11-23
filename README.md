# SPECTRA / 多光谱与 HDR 图像合成工作台

面向游戏与互动媒体素材的本地多光谱、HDR 合成工作台。工程从零实现，只依赖 Go 标准库；样本、真值、处理、分段状态机、检查点、HTTP API 与浏览器界面均在同一仓库内。

## 快速开始

环境要求：

- Go 1.22 或更高版本。
- 本地可写目录（默认 `./data`）。
- 支持现代 JavaScript 的本地浏览器。
- 不需要 Docker、容器、云端服务或外网依赖。

```bash
go build ./...
go vet ./...
go test ./...
go run ./cmd/workbench -addr 127.0.0.1:8080 -data ./data
```

浏览器打开 `http://127.0.0.1:8080`。

Windows 目标交叉编译（本机已验证）：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

当前验收机运行时为 `linux/amd64`、Go `1.22.12`，`CGO_ENABLED=1`；实现本身不引用 CGO，因此 Windows 交叉编译和纯静态使用不依赖本地 C 工具链。所有持久化写入只使用标准库文件 API，并为 Windows 下“目标文件已存在”的 rename 行为提供了串行回退路径。

## 功能地图

```text
cmd/workbench/                 本地 HTTP 服务入口
pkg/workbench/
  generator.go                 确定性场景、曝光栈、细节层与真值生成
  validate.go                  E_META / E_BAND / E_ORDER 加载契约
  register.go                  Lagrange 可配置阶数配准
  fusion.go                    逆相机响应、线性化、过/欠曝权重
  spectral.go                  CIE XYZ、白点适配、目标色域、色域压缩
  process.go                   单像素与分块确定性内核
  scale.go                     自适应路径、E_SCALE 比对
  state.go                     四态分段状态机
  checkpoint.go                原子串行检查点与整体失效
  engine.go                    工作池、暂停/续跑、冻结、故障隔离
  server.go                    HTTP API、PNG、直方图、色度、导出
  web/                         自带离线交互界面
```

## 确定性样本与真值

`GenerateScene` 使用自实现的 SplitMix64 与二维整数哈希，不依赖 `math/rand` 运行时序列。同一配置和种子在任意 Go 版本、线程调度下生成相同样本。

默认样本：

- `96×72` 像素，3 个波段，3 档曝光。
- 波段中心波长 `450 / 550 / 650 nm`，FWHM `38 nm`。
- 曝光时间 `1/250 / 1/60 / 1/12 s`，严格递增。
- `16 bit` 量化，`3.76 µm` 像素间距，D65 白点。
- 6 个已知色块、波段间亚像素配准偏移、峰值辐亮度真值。

真值只由生成器的解析场景构造计算，不读取处理输出：

- 每个色块中心的目标线性 RGB。
- 场景 `Y + detailStrength × detail` 的峰值辐亮度。
- 每个非参考波段相对 `b0` 的 `(dx, dy)` 位移。
- 目标色域三角形边界 xy 坐标、容差声明。

内置预设：

- `demo`：`96×72`、3 波段。
- `multi6`：`144×108`、6 波段、4 曝光。
- `scale8`：`1024×720`、8 波段、4 曝光；自动进入分块/降采样路径。

## 处理链

### 1. 加载校验

`ValidateScene` 会在处理前检查：

- 尺寸、位深、像素间距、逆相机响应、辐射缩放、目标色域、波段元数据版本、白点。
- 波段图像数量、波段中心波长、曝光帧数量、帧时间与元数据。
- 曝光时间严格单调递增。
- 白点在元数据与真值中完全一致。
- 量化样本不超过声明位深。

错误结构统一为 `WorkbenchError`，包含错误码、字段名、实际值、期望值、波段对、偏移、像素坐标、通道和分段编号：

| 错误码 | 触发场景 |
|---|---|
| `E_META` | 缺失/非法元数据、尺寸/位深/白点/像素数据不一致 |
| `E_BAND` | 波段数量或波段基数不匹配 |
| `E_ORDER` | 曝光时间非严格递增 |
| `E_REG` | 配准偏移超出可校正范围 |
| `E_PARA` | 插值阶数、权重、细节强度、色域意图等参数越界 |
| `E_GAMUT` | 严格意图下越界，或像素/通道无法表达 |
| `E_SCALE` | 全量与分块路径逐像素、峰值或色域集合超差 |
| `E_STATE` | 非法分段状态迁移或非完整结果请求 |
| `E_IO` | 持久化/导出读写失败 |
| `E_INTERNAL` | 单段内部异常或 panic |

### 2. 配准与线性化

- 配准采用确定性 Lagrange 插值，阶数 `1..5`，默认 3。
- 采样位置为 `x-dx, y-dy`；整数偏移严格复现，分数偏移使用固定 stencil。
- 边缘坐标 clamp，最终辐亮度非负。
- 超出 `maxRegistrationOffsetPx` 返回 `E_REG`，其中包含 `bandPair` 和实测最大偏移。
- 曝光线性化：`L = (code / (2^bitDepth-1))^gamma / exposureTime`。
- 所有像素计算均为纯函数；`TestThreadSchedulingDeterminism` 比较单线程与四线程结果，融合辐亮度和 RGB 最大差均为 0。

### 3. 曝光融合

权重函数：

```text
reliability = (1 - |2c - 1|^4)^sharpness
bias        = exp(6 * weightBias * (c - 0.5))
weight      = reliability * bias
```

`c` 是归一化编码值。接近 0 或 1 的过曝/欠曝样本可靠性趋近 0，因此显著降权；若全部权重耗尽则退回中间曝光。

融合结果增加 `detailStrength × detailLayer`，并保持非负。默认样本的峰值相对真值误差约 `1.35e-5`。`weightBias` 在 `-0.8..0.8` 的五个采样点上产生单调可测的平均辐亮度响应。

### 4. 光谱重建与目标色域

- 使用 CIE 1931 2° 解析近似和 D65/Planckian 照明，在 `380..780 nm`、`5 nm` 步长上构造波段响应基。
- 先预积分得到每个波段到 XYZ 的 `B×3` 矩阵；等价于逐像素执行波段响应与 CIE CMF 的积分离散求和。
- 采用 Bradford 变换完成白点适配。
- 目标色域支持 `sRGB`、`DisplayP3`、`Rec2020`。
- `compress` 意图沿中性轴方向向色域边界压缩，不做逐通道硬裁剪；所有越界像素被记录到可观测集合，并输出色域边界 xy。
- `strict` 意图遇到第一个越界像素返回 `E_GAMUT`，包含像素坐标与通道。
- 默认样本 6 个真值色块的最大误差约 `5.2e-4`，远低于声明容差 `0.035`。

### 5. 自适应路径

`DecideScale` 依据以下信息生成可解释决策：

- 有效工作量 = `像素数 × 波段数`。
- 工作集估算 = 波段、曝光、细节、融合、RGB、显示缓冲与掩码。
- 内存预算、时间预算、每线程像素缓冲上限。

当前规则：

- `effectivePixels > 4,000,000` 时选择分块。
- 估算内存超过预算 75% 时选择分块。
- 全量估算时间超过时间预算时选择分块。
- 小场景留全量高精度路径。
- 大规模场景将块面积扩展到最多 `65,536 px`，减少检查点数量和调度开销。
- 分块降采样倍率由有效工作量和目标粗分析规模自动计算，并在决策中记录。

降采样用于粗曝光统计、调度和路径诊断；融合辐亮度、色域映射与显示编码在块内按原生分辨率精化。这样既约束了工作集，又避免把双线性上采样误差带入最终像素。两条路径共享完全相同的像素内核，因此最终图逐像素一致。

`CompareRenderResults` 检查：

- 融合辐亮度逐像素最大绝对差。
- 线性 RGB 逐像素最大绝对差。
- 显示编码逐像素最大绝对差。
- 峰值差。
- 色域越界集合哈希。

超差返回 `E_SCALE`，带首个不一致像素、分段编号，以及 `fusedMaxAbs`、`rgbMaxAbs`、`gamutSetDiff` 三项核心实测值（另附 `displayMaxAbs`）。

## 分段、状态与冻结

分段按固定块尺寸创建，状态迁移如下：

```text
pending    -> processing
processing -> committed
processing -> failed
failed     -> pending
committed  -> 不允许离开
```

非法迁移返回 `E_STATE`，原状态不变。进度始终是“已提交段数 / 总段数”。

每个提交段冻结：

- 融合辐亮度。
- 色度缓冲（目标线性 RGB）。
- 显示编码结果。
- 色域越界掩码。
- 参数快照和参数版本。
- 波段元数据版本、算法版本、路径与降采样倍率。
- 内容 SHA-256。

参数更新只影响尚未开始的段；处理中的段保留启动时快照；已提交段不再读取新参数。

### 工作池与缓冲

- 最大并行度可配置为 `1..16`。
- 单段像素面积不超过每线程像素缓冲上限。
- 所有线程缓冲被占用时，后续段进入队列，并记录：
  `all per-thread pixel buffers are in use; segment queued`。
- 单段错误或 panic 只将该段置为 `failed`，其他已提交段保持不变。
- `Step` 可逐段提交；`Pause` 停止派发新段但允许在途段完成；`Resume` 继续。

## 持久化检查点

默认位置：`<data>/checkpoints/<scene-id>/`。

每段包含一个 gob 检查点和 manifest 条目：

- 段矩形与像素缓冲。
- 融合辐亮度、色度缓冲、显示缓冲、色域掩码。
- 峰值、均值、越界数量与样本。
- 内容哈希。
- 波段元数据版本、参数版本、算法版本。
- 路径与降采样倍率。

写入顺序：

1. 在进程内 checkpoint mutex 下串行化。
2. 写入同目录临时文件并 `fsync`。
3. rename 为正式段文件。
4. 更新 manifest。

启动加载时：

- manifest 的场景 ID、种子、波段元数据版本、算法版本必须匹配。
- 每个段文件的场景 ID、三段版本、参数版本、尺寸、缓冲长度与 manifest 一致。
- 重新计算内容哈希，必须与 manifest 和段头一致。
- 任一项失败即删除整个场景快照并全量重算，不允许部分恢复。

注意：当前参数版本与已冻结段不同不会单独导致失效，因为这些段的设计语义就是冻结在旧版本；manifest 与段头记录的版本必须互相一致。

## HTTP API

所有成功响应为：

```json
{"ok":true,"data":{}}
```

错误响应：

```json
{"ok":false,"error":{"code":"E_PARA","field":"interpolationOrder","actual":9,"expected":"1..5"}}
```

主要端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/health` | 运行时、算法和依赖信息 |
| `GET` | `/api/presets` | 内置确定性场景预设 |
| `GET` | `/api/scenes` | 场景与引擎状态 |
| `POST` | `/api/scenes` | 按 `preset` 或 `GenerateConfig` 创建场景 |
| `GET` | `/api/scenes/{id}/status` | 分段矩阵、日志、进度、检查点、策略 |
| `POST` | `/api/scenes/{id}/start` | 启动工作池 |
| `POST` | `/api/scenes/{id}/pause` | 暂停派发新段 |
| `POST` | `/api/scenes/{id}/resume` | 续跑 |
| `POST` | `/api/scenes/{id}/step` | 串行提交一个段 |
| `POST` | `/api/scenes/{id}/retry` | `{"index":1,"clearFault":true}` 重试失败段 |
| `POST` | `/api/scenes/{id}/params` | 更新未提交段参数 |
| `POST` | `/api/scenes/{id}/fault` | 诊断用故障注入 |
| `GET` | `/api/scenes/{id}/result` | 完整结果、真值校验、分段摘要 |
| `POST` | `/api/scenes/{id}/compare` | 实时/恢复/全量/分块逐像素比对 |
| `GET` | `/api/scenes/{id}/histogram?kind=fused` | 融合、RGB 或细节直方图 |
| `GET` | `/api/scenes/{id}/chromaticity` | xy 色度示波器数据 |
| `GET` | `/api/scenes/{id}/image/{kind}` | `bandN`、`fused`、`rgb`、`gamut`、`detail`、`diff` PNG |
| `GET` | `/api/scenes/{id}/export` | ZIP 导出 |

创建大规模预设并查看自动决策：

```bash
curl -s -X POST http://127.0.0.1:8080/api/scenes \
  -H 'Content-Type: application/json' \
  -d '{"preset":"scale8"}'
```

启动、暂停、续跑：

```bash
SID=scene-20261126-1024x720-b8-e4-1a656602
curl -s -X POST http://127.0.0.1:8080/api/scenes/$SID/start
curl -s -X POST http://127.0.0.1:8080/api/scenes/$SID/pause
curl -s -X POST http://127.0.0.1:8080/api/scenes/$SID/resume
```

三路径比对与导出：

```bash
curl -s -X POST http://127.0.0.1:8080/api/scenes/$SID/compare
curl -s -o spectra.zip http://127.0.0.1:8080/api/scenes/$SID/export
```

## 界面

界面完全由仓库内静态资源提供，不访问 CDN：

- 场景预设库与确定性 manifest。
- 分段状态矩阵，失败段可点击重试。
- 波段源图、融合辐亮度、线性 RGB、色域掩码、细节层、差异热力图。
- 融合/RGB 直方图。
- xy 色度示波器与目标色域边界。
- 参数机架、自适应策略解释、检查点状态与事件流。
- 启动、暂停、续跑、步进、三路径比对、ZIP 导出。

一键演示（启动服务、创建 `scale8`、暂停、续跑、比对并导出）：

```bash
./scripts/demo.sh
```

## 验证记录

以下结果来自本机实际执行（Go `1.22.12 linux/amd64`）。

### 构建与静态检查

```text
go build ./...                              PASS
go vet ./...                                PASS
go test ./...                               PASS
go test -race ./pkg/workbench               PASS
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...  PASS
```

测试覆盖：

- 生成器 JSON 往返、`E_META` / `E_BAND` / `E_ORDER`。
- 配准 `E_REG` 携带波段对/偏移、插值阶数 `E_PARA`。
- 过曝/欠曝权重抑制、权重偏置单调响应、细节强度残差。
- 严格色域 `E_GAMUT` 的像素/通道。
- 单线程 vs 四线程逐像素确定性。
- 全量与分块 `E_SCALE` 首像素/块号和三项指标。
- 状态机非法迁移保持原状态。
- 提交段参数冻结。
- 检查点恢复、篡改后整体失效、三路径一致。
- 单段故障隔离和重试。
- HTTP 场景创建、处理、结果、PNG、比对、ZIP 导出、结构化 API 错误。

### 默认样本

```text
Scene: scene-20261124-96x72-b3-e3-efcfba88
Path:  segmented-full, tile 64×64, downsample ×1
Truth peak:    5.4190971663
Measured peak: 5.4191703318
Peak relative error: 1.350e-5  (tolerance 2.5e-2)
Max patch error:     5.19e-4  (tolerance 3.5e-2)
OOG set: 1597 pixels, SHA-256 observable
```

单线程与四线程对照：融合辐亮度和 RGB 最大差均为 `0`。

### 大规模自动路径

```text
Scene: scene-20261126-1024x720-b8-e4-1a656602
Pixel count: 737,280
Effective band-pixels: 5,898,240
Selected path: tiled
Tile: 256×256 (65,536 px, within 262,144 px/thread limit)
Downsample: ×2
Coarse analysis: 184,320 sampled pixels, peak 5.460684, mean 0.834503
Segments: 12
Decision: effective band-pixels exceed 4,000,000 threshold
```

暂停验证：启动后暂停，状态为 `5/12` 已提交、`7` 待处理、`0` 处理中、`0` 失败；工作缓冲竞争日志出现 `all per-thread pixel buffers are in use; segment queued`。续跑后 `12/12` 完成。

续跑结果：

```text
Truth peak:    5.4877140012
Measured peak: 5.4883382993
Peak relative error: 1.138e-4
Max patch error:     6.88e-4
Checkpoint restored: 12/12 segments
```

三路径与分块对照：

```text
live ↔ full:     fusedMaxAbs=0, rgbMaxAbs=0, displayMaxAbs=0, gamutSetDiff=0
resume ↔ full:   fusedMaxAbs=0, rgbMaxAbs=0, displayMaxAbs=0, gamutSetDiff=0
live ↔ resume:   fusedMaxAbs=0, rgbMaxAbs=0, displayMaxAbs=0, gamutSetDiff=0
full ↔ tiled:    fusedMaxAbs=0, rgbMaxAbs=0, displayMaxAbs=0, gamutSetDiff=0
Peak:            live=resume=full=tiled=5.4883382993
Gamut set hash:  live=resume=full=tiled
```

### 异常与导出链路

- 对默认样本第 1 段注入 `http diagnostic fault`：仅该段变为 `failed`，其余 3 段保持 `committed`。
- 清除故障并重试后恢复为 `4/4` 全部提交。
- ZIP 实际条目：`manifest.json`、`truth.json`、`segments.json`、`histogram-fused.json`、`fused.png`、`rgb.png`、`gamut.png`、3 个波段 PNG、`README.txt`。

## 设计取舍

- 真值使用生成器解析场，不把管线输出回写为真值。
- 检测器采用解析 CIE 近似，避免依赖外部光谱数据文件，保证完全离线与可复现。
- 大规模路径把降采样用于粗统计而不是最终色度上采样；最终像素在块内原生精化。这一取舍以更强的一致性换取了可解释的块级内存上限。
- 检查点以“快照整体有效”为原则：不尝试跳过坏段，任何不一致都全量重算，降低恢复结果与实时结果分叉的风险。
- 工作池只并行独立分段；已提交快照不再参与写路径，避免参数更新与检查点写入交错。
