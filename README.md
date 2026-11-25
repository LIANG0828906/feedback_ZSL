# SPECTRA — 多光谱与 HDR 合成工作台

SPECTRA 是一个从零实现、可本地重复运行的 Go 工作台，用于游戏与互动媒体美术素材的多光谱 / HDR 合成。它内置固定种子的场景生成器、三版场景描述互操作、配准、逆响应线性化、曝光融合、XYZ 色度重建、分块状态机、SQLite 检查点、精确增量失效和浏览器交互应用。

运行时不访问外部文件或服务。所有样本、真值、色块、峰值辐亮度与配准偏移均由生成器构造。

## 快速开始

要求 Go 1.22 或更高版本，SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`，不需要 CGO。

Windows PowerShell：

```powershell
go version
go env
go test ./...
go run ./cmd/workbench demo -out artifacts/demo-report.json
go run ./cmd/workbench serve -addr 127.0.0.1:8080 -db-dir artifacts
```

Linux / macOS：

```bash
go test ./...
go run ./cmd/workbench demo -out artifacts/demo-report.json
go run ./cmd/workbench serve -addr 127.0.0.1:8080 -db-dir artifacts
```

浏览器打开 `http://127.0.0.1:8080`。应用默认加载 `neon-foundry` v3，并以断点恢复模式处理 12 个固定像素分段。

从完全空目录复现模块初始化：

```bash
go mod init ms-workbench
go get modernc.org/sqlite@v1.34.5
```

## 命令

| 命令 | 用途 |
| --- | --- |
| `go run ./cmd/workbench serve` | 启动交互式工作台 |
| `go run ./cmd/workbench demo` | 执行 schema、真值、精度和三路径验收，输出 JSON 报告 |
| `go run ./cmd/workbench samples -dir artifacts/samples` | 写出两套场景的 v1/v2/v3 文件与 truth |
| `go run ./cmd/workbench run -scene neon-foundry -schema 3 -mode full` | 无界面运行单个场景 |

常用参数：

```text
serve   -addr 127.0.0.1:8080 -db-dir artifacts -seed 20261125 -workers 4 -pixel-buffer 1024 -chunk 16
demo    -seed 20261125 -out artifacts/demo-report.json
run     -scene neon-foundry -schema 1|2|3 -mode live|resume|full -db artifacts/run.db
```

## 交互应用

应用包含：

- 分段状态矩阵：`pending / processing / committed / failed` 四态、重用标记、失败原因和像素块坐标。
- 已提交分段进度、暂停、续跑、逐段重试。
- 五个光谱波段与融合合成结果并排检查，越界像素以洋红色叠加。
- 各波段辐亮度直方图与合成亮度直方图。
- 构造色块真值表：期望 RGB、实测 RGB、最大通道误差与容差判定。
- 三路径验证按钮与逐像素差异热力图；正常情况下显示 `0 DIFFERING PIXELS`。
- 参数编辑与波段图像替换；界面显示分段编号、失效阶段、失效原因和缓存复用状态。
- SQLite 快照来源信息：场景指纹、参数版本、波段元数据版本、算法版本和提交序列。
- JSON 导出接口，包含按分段顺序拼接的结果、PNG Base64、峰值辐亮度、越界集合、色域边界坐标和色块测量。

主要 API：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/scenes` | 内置场景和版本列表 |
| `POST` | `/api/session` | 加载指定场景/schema/运行路径 |
| `GET` | `/api/session` | 当前状态、分段、真值和直方图 |
| `POST` | `/api/session/pause` | 在批次边界暂停 |
| `POST` | `/api/session/resume` | 续跑未提交分段 |
| `POST` | `/api/session/retry` | 重试失败分段 |
| `POST` | `/api/session/settings` | 修改参数并执行精确失效 |
| `POST` | `/api/session/band` | 替换波段图像并执行局部失效 |
| `POST` | `/api/verify` | 隔离运行三条路径并逐像素比较 |
| `GET` | `/api/image` | 合成、波段或差异热力图 PNG |
| `GET` | `/api/histogram` | 分段提交后的直方图 |
| `GET` | `/api/export` | 有序导出结果与测量数据 |

## 三版 Schema

三个版本编码同一份内部语义模型，但字段结构明显不同：

| 差异 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 顶层版本 | `schema_version: 1` | `schema: "scene/v2"` | `schemaVersion: 3` |
| 尺寸 | `pixel_size{w,h}` | `dimensions{width,height}` | `size:[w,h]` |
| 波段元数据 | `bands[].center_nm` / `response_xyz` | `bandMeta[].metadata.centerWavelength` / `responseXYZ` | `wavebands[].centerWavelength` / `xyzResponse` |
| 曝光帧 | 波段内 `exposures[].duration_us` | 顶层 `capture.frames[].duration{value,unit}` | 顶层 `captures[].exposure.seconds` |
| 时间单位 | 整数微秒 | `us` 或精确十进制秒 | 精确十进制秒 |
| 白点 | `white_point{x,y,z}` | `whitePoint{X,Y,Z}` | `white[3]` |
| 渲染字段 | 无，使用默认值 | 插值、配准、融合四项 | 增加色域意图、目标色域和细节增益 |

加载后 `SourceVersion` 保留源版本，`SchemaVersion` 表示当前内存表示版本。未知版本返回 `E_VER`。缺失必填字段返回 `E_SCHEMA`，错误对象含字段路径与源版本。

升级为低版本缺失的新字段时，使用声明默认值补齐，并在 `ConversionTrace` 中记录：

```text
action / fromVersion / toVersion / path / oldValue / defaultValue / defaultSource
```

降级移除不可表达字段，生成 `E_LOSSY` 损失清单，包含字段路径、旧值、目标版本、处置方式以及是否发生语义损失。重复导出同一内部模型得到逐字节一致的结果。生成的 v1 场景升级到 v3 再降级回 v1 后，重新升级的语义设置逐项相等。

## 处理链

1. **多波段配准**：按波段相对参考波段的声明偏移做确定性 Lagrange 插值，阶数 0–5 可配。偏移超限返回 `E_REG`，错误含波段对、X/Y 偏移、实测模长和限制值。
2. **辐射线性化**：量化值先扣除黑电平，再按声明的逆相机响应指数线性化；曝光时间在加载时全部转换为整数微秒，线性化换算为 `fullWell * 1_000_000 / durationUS`。
3. **曝光融合**：每一帧按码值中点权重函数融合，`fusionPower` 和 `fusionStrength` 可调；接近黑场/饱和的像素权重额外乘以 `0.0005`，避免过曝与欠曝污染。
4. **细节层**：对齐后的噪声细节按 `detailGain` 注入融合辐亮度。
5. **XYZ 色度重建**：五波段辐亮度与声明响应积分得到 XYZ，使用 Bradford 做白点适配，再进入目标 sRGB 矩阵。
6. **色域映射**：`relative-colorimetric`、`perceptual`、`saturation` 三种意图都执行压缩。原始越界 RGB、压缩后 RGB、像素坐标与全局越界集合均可观测。

生成器为每个色块构造三种意图各自的期望 RGB，因此切换声明意图不会被误判为精度失败。

## 分段状态机

```text
pending ──► processing ──► committed
                 │              │
                 ├──► failed    └──► pending   (显式依赖失效)
                 │       │
                 │       └──► processing      (逐段重试)
                 └──► pending                  (暂停/回滚)
```

非法迁移返回 `E_STATE`，并保留原状态。已提交缓冲不做原地修改；只有显式依赖失效能把提交段转为待处理。融合阶段失效会丢弃融合与色度缓冲；只有下游色度阶段失效时，已提交的融合辐亮度仍在内存中复用，仅重算 XYZ/色域阶段。

## SQLite 快照

每个提交分段在单一事务中写入：

- 融合辐亮度 `[]float64`
- 色度 / 线性 RGB 缓冲 `[]float64`
- 越界像素集合与色域边界
- 分段峰值辐亮度
- 内容 SHA-256
- 波段元数据版本、参数版本、算法版本
- 稳定分段编号与提交事件

所有写入经同一互斥锁和单连接串行化，每个分段独立原子提交。最终逻辑提交序列按分段编号稳定排序。

重启加载执行全快照校验：全局场景指纹、几何、参数/波段/算法版本、每一行内容哈希、分段顺序与完成标志。任意一项不匹配都会删除整个快照并全量重算，绝不混用旧分段。增量编辑只删除实际失效行，其余有效行在新全局版本下重新盖章。

## 精确失效与三路径

显式依赖关系通过 `Engine.Dependencies()` 暴露，并随状态 API 返回。规则包括：

- 保留融合细节参数：全部分段的融合 + 下游色度。
- 色域意图 / 目标色域：全部分段的色度阶段。
- 波段响应：全部分段的色度阶段。
- 波段配准、曝光时间：全部分段的融合 + 色度。
- 波段图像：仅与变化像素（按配准与插值支撑扩展后）相交的分段。

失效报告逐段包含分段编号、失效阶段、原因和复用状态。

三条路径为：

- **realtime**：工作线程池逐批处理当前内存状态。
- **resume**：先持久化部分分段，关闭 SQLite，再重新打开并续跑。
- **full**：清空快照并一次全量重算。

`engine.Verify` 在三个隔离数据库中分别运行三条路径，用浮点位模式比较每个融合辐亮度与线性 RGB 值，并比较峰值辐亮度、越界集合和提交序列。首次不一致会给出像素坐标、分段编号、字段及通道。

## 并发与内存

引擎限制 `MaxWorkers`、`MaxPixelsPerWorker` 和总像素缓冲。每批最多调度最大并行度数量的分段；每个分段必须满足单线程像素上限。像素信号量按实际像素数获取，缓冲不足时阻塞等待而不是丢弃分段。共享 SQLite 写入串行；导出与合成缓冲按分段编号拼接。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_VER` | 未知或不支持的 schema 版本 |
| `E_SCHEMA` | 必填字段缺失或结构非法，带字段路径与源版本 |
| `E_LOSSY` | 导出低版本时存在不可表达的字段，带损失清单 |
| `E_REG` | 配准偏移超出可校正范围，带波段对与实测偏移 |
| `E_STATE` | 非法分段状态迁移或运行状态冲突 |
| `E_HASH` | 检查点内容哈希损坏，整快照作废 |
| `E_VERSION` | 快照版本/场景不匹配，整快照作废 |
| `E_PARAM` | 参数、几何或工作池配置非法 |
| `E_IO` | SQLite、文件或 PNG 编码故障 |

## 实际验证结果

本机环境：Go `1.22.12`，Linux amd64。另已实际交叉编译 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 成功。

```text
$ go test ./...
cmd/workbench              no test files
internal/engine            ok
internal/pipeline          ok
internal/scene             ok
internal/server            ok

$ go test -race ./internal/engine ./internal/server
internal/engine            ok
internal/server            ok

$ go run ./cmd/workbench demo -out artifacts/demo-report.json
schema  source  bytes  bands  settings  size
v1      true    true   true   true      64x48
v2      true    true   true   true      64x48
v3      true    true   true   true      64x48
patches 7/7 pass, max delta 0.00048181, peak truth 3.000000 (delta 0.00016720)
paths live=[0 1 2 3 4 5 6 7 8 9 10 11]
      resume=[0 1 2 3 4 5 6 7 8 9 10 11]
      full=[0 1 2 3 4 5 6 7 8 9 10 11]
exact=true, max pixel delta=0.00000000
resume loaded checkpoint=true, commit sequences equal=true
acceptance: true
```

完整机器可读报告见 `artifacts/demo-report.json`。测试还覆盖：未知版本、缺失字段路径、升级痕迹、损失清单、重复导出字节一致、秒/微秒整型换算、配准越界上下文、三种色域意图真值、非法状态迁移、失败段重试、局部波段替换、增量与全量一致、损坏哈希整库作废、版本不匹配整库作废、HTTP 应用生命周期及三路径验证。

## 目录

```text
cmd/workbench/          CLI 入口
internal/apperr/        稳定错误码与损失对象
internal/color/         Bradford、XYZ、色域压缩
internal/scene/         统一模型、v1/v2/v3 编解码、转换与真值类型
internal/samples/       固定种子场景生成器
internal/pipeline/      配准、线性化、融合、色度、色块测量
internal/engine/        状态机、工作池、失效、三路径验证
internal/store/         modernc SQLite 快照与全量完整性校验
internal/server/        HTTP API、PNG/直方图与内嵌 Web 应用
```

本项目不使用 Docker、容器、外部服务或远程目标。
