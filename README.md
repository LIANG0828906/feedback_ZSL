# spectralrender

面向游戏与互动媒体光谱材质的本地 Go 渲染组件。仓库包含确定性样本生成器、严格场景校验、光谱积分与色域转换、定点量化、分块并行渲染、版本化预览/精确帧、原子检查点、独立调试校验器和本地 HTTP 诊断面板。

- 模块：`spectralrender`
- Go：1.22+
- 外部依赖：无
- 样本目录：`samples/`（可重复生成）
- 检查点目录：`checkpoints/`（工作目录下固定子目录）

## 快速开始

```bash
go version
go mod init spectralrender   # 在本仓库已执行，首次从空目录创建时使用
go run ./cmd/samplegen -out samples -seed 42
go test ./...

# 渲染、写 PNG 并对照真值
go run ./cmd/render -samples samples -checkpoints checkpoints -out out

# 仅凭检查点、场景和快照参数重算块/图像并校验上界
go run ./cmd/verify -samples samples -checkpoints checkpoints

# 回环地址诊断面板
go run ./cmd/server -addr 127.0.0.1:8080 -samples samples -checkpoints checkpoints
```

打开 `http://127.0.0.1:8080/`。服务默认只绑定回环地址。面板支持连续预览（每次读取最新参数快照）、精确渲染（启动时锁定版本）、独立块校验和 PNG 查看。

也可使用：

```bash
make sample
make test
make render
make verify
make serve
```

## 目录

```text
.
├── scene.go            场景/真值严格 JSON 解析与字段路径校验
├── sampling.go         波长采样网格、rectangle/trapezoid/simpson 权重
├── engine.go           逐材质/逐光源光谱积分预计算、白点归一、色域矩阵
├── quantization.go     定点量化、逐样本误差、内容+量化缓存键
├── render.go           光栅化、分块、直接光/间接反弹、色调映射、版本检查
├── checkpoint.go       原子块快照、清单、恢复与整体失效
├── verify.go           独立重算、首个差异定位、逐像素误差上界
├── truth_check.go      真值容差/峰值/色度检查
├── samplegen.go        固定种子场景、CMF、真值生成
├── cmd/samplegen       样本生成命令
├── cmd/render          单帧渲染命令
├── cmd/verify          检查点调试校验命令
├── cmd/server          本地 HTTP 示例与诊断面板
├── samples/            已生成的确定性 scene/truth/manifest
└── checkpoints/        运行后产生的固定持久化目录（生成物）
```

## 样本与数据模型

`go run ./cmd/samplegen -out samples -seed 42` 写出：

- `samples/scene.json`：96×64 画面、三组网格、每顶点定点反照率/粗糙度/金属度、三盏逐波长光源、每种材质的基函数与定点系数、1 nm 色匹配函数表、白点与线性 sRGB 色域矩阵。
- `samples/cmf.json`：独立导出的 1 nm 色匹配函数表、白点和目标色域说明。
- `samples/truth.json`：六个已知像素的高精度参考线性 RGB、峰值辐亮度、色度坐标及容差/偏差上界。
- `samples/manifest.json`：种子、文件名、场景/CMF/真值文件哈希。

当前样本身份：

```text
scene_hash 267010c60b92550c73583859c39d91b1fa8cdcdb4230a1064672b12b124abcdf
cmf_hash   c689576276e8c2df98defa12cc9c3dff6b1392518dd29be66aec94d0388b22d0
truth_hash bb3636fb4c8030626af2c7064647ebdb4ed8bf1fd979e9fd1d670a8a3288fffc
```

场景中的每个顶点只保存声明量化单位下的整数；材质反射率为

```text
R(λ) = clamp(Σ coefficient_i / coefficient_unit · basis_i(λ), 0, ...)
```

渲染器按采样网格对光源、基函数和 CMF 线性插值，逐点计算 `light(λ)·R(λ)·CMF(λ)`，再按所选数值积分得到 XYZ。随后按场景白点归一，乘以 `xyz_to_rgb` 矩阵得到目标色域线性 RGB。直接光按每盏灯的法线余弦加权；间接分量使用可配置次数和衰减系数的确定性反弹近似。

支持的积分方式：

- `rectangle`：左端矩形/一阶
- `trapezoid`：复合梯形/二阶
- `simpson`：复合 Simpson/四阶；偶数采样点最后一个区间使用梯形

采样数允许 `[4,4097]`，积分或采样参数越界返回 `E_PARA` 并带字段名。

## 定点量化与缓存

默认使用场景声明的 10 位顶点量化和 12 位材质系数量化。`RenderParams.VertexBitsOverride` / `CoefficientBitsOverride` 可显式降低或提高目标精度：

- 低于源精度时，按目标单位最近舍入。
- 不低于源精度时保留源值，避免“提高位深反而引入二次舍入误差”。
- `QuantizationReport.Samples` 记录每个字段的 `before/after/error`，并给出 `max_abs`、`mean_abs`、`RMSE`；测试验证 4→12 位误差统计单调不增。
- 材质缓存键由场景内容哈希、场景哈希、算法版本和有效量化参数共同决定。量化参数改变会产生新键和新检查点快照，旧已提交块只读保留，不会被静默覆盖。

## 并行、顺序提交与一致性

- 固定屏幕块尺寸，默认 `16×16`，当前样本共 24 块。
- `max_parallel` 限制并发工作块数；`memory_budget_bytes / tile_bytes_estimate` 限制并发块缓冲数。
- 缓冲或工作槽不足时阻塞排队，`RenderReport.SchedulerEvents` 记录等待块号与原因，不会静默丢块。
- 结果按块号顺序串行提交；并发只影响计算时序，不影响块内容或提交顺序。
- 测试逐位比较串行、并行、检查点恢复三条路径的 `LinearRGB` 与编码字节。

## 参数版本与预览/精确帧

- `ParamStore.Snapshot()` 返回不可变参数快照。
- 连续预览在请求开始时读取最新快照，并只使用该快照完成整帧。
- `BeginExact` 在精确渲染启动时锁定版本；后续预览更新不会混入精确帧。
- `EncodeDisplay` 在组合线性帧和曝光/曲线参数时检查版本；不一致返回 `E_VER`，`versions` 字段同时给出两个版本号。
- 相同曝光与曲线参数下，预览和精确帧使用同一编码函数，像素逐位一致。

## 检查点与重启

`checkpoints/` 下结构：

```text
checkpoints/
├── current.json                    当前活动快照指针
└── snapshot-<identity>/
    ├── manifest.json               场景哈希、采样版本、参数版本、材质键、块清单
    ├── tiles/tile-000000.json      每块线性/XYZ/峰值累积、摘要、版本元数据
    └── image.json                  完整帧线性缓冲、编码字节和摘要
```

提交顺序：

1. 校验块尺寸、非有限值和块摘要。
2. 在同目录创建临时文件、写入、`fsync`、`rename` 成正式块文件。
3. 再原子更新 `manifest.json`。
4. 全部块完成后写 `image.json` 并标记 `complete`。

进程在步骤 2 与 3 之间被强制终止时，最坏只会留下未入清单的孤儿块，恢复时不会读取。块提交由单一串行提交器完成。

恢复时比较场景内容哈希、采样版本、参数版本、参数摘要、材质缓存键、算法版本和块布局。任一不匹配则整个旧活动快照作废，所有块全量重算；旧目录保留为不可变历史，不与新块混合。相同快照中已存在的块若摘要不同，提交会被拒绝而不是覆盖。

## 独立调试校验与误差上界

`cmd/verify` 和 `VerifyCheckpoint` 不信任块缓冲：

1. 从 `manifest.json` 读取参数、版本和块清单。
2. 只使用场景清单 + 快照参数重新光栅化、积分、着色和色调映射。
3. 对每块先比较 `linear_rgb`、`xyz_accum`、`peak_radiance`，再比较块摘要。
4. 汇总重建最终图像，比较 `image.linear_rgb`、编码字节和编码摘要。
5. 首个不一致输出块号、像素坐标、字段名、期望值和实际值。

逐像素上界由当前参数推导：

```text
order = 1 (rectangle) | 2 (trapezoid) | 4 (simpson)
h = 1 / (sample_count - 1)
sampling = sampling_constant * h^order
bounce_factor = 1 + sum(indirect_weight^i, i=1..bounces)
quantization = quantization_constant * (1/vertex_unit + basis_count/coefficient_unit) * bounce_factor
linear_rgb_bound = sampling + quantization
peak_bound = peak_scale * linear_rgb_bound
chromaticity_bound = chroma_constant * linear_rgb_bound + 1e-4
```

校验器把该上界复制到每个像素（报告中的 `per_pixel_*` 数组），并检查真值像素的 RGB 绝对误差、峰值误差和色度误差的 95% 分位是否都不超过对应上界。若上界低于实测分位，返回 `E_BOUND`。默认安全系数可通过 `cmd/verify` 的 `-sampling-constant`、`-quantization-constant`、`-peak-scale`、`-chroma-constant` 调整。

## 错误码

| 错误码 | 触发条件 |
|---|---|
| `E_SCHEMA` | 缺字段、非整型定点字段、字段路径/长度/单调性/量化单位错误 |
| `E_DECODE` | 场景、光谱表、真值或检查点 JSON 不可读/损坏 |
| `E_PARA` | 采样步数、积分方式、曝光、曲线、预算等参数越界 |
| `E_VER` | 线性帧/参数/块/图像版本混用，`versions` 给出两个版本号 |
| `E_GAMUT` | 目标色域出现负通道或非有限值，带像素与通道 |
| `E_BOUND` | 推导上界低于真值实测 95% 分位 |
| `E_RENDER` | 单块注入/内部异常；已提交块保留，可重试 |
| `E_CANCEL` | 用户取消；只承诺已原子提交块可恢复 |
| `E_STATE` | 快照不完整、缺块、最终图像不存在或内部状态不一致 |
| `E_CHECKPOINT` | 检查点目录/原子写/不可变块冲突等存储错误 |

## 本次实际验证

环境：`go1.22.12 linux/amd64`。

```text
go test ./...        PASS
go test -race ./...  PASS
samplegen             scene_hash=267010c6..., cmf_hash=c6895762..., truth_hash=bb3636fb...
render 96x64 v2       encoded_sha256=7fcdd239811708c287db15a4dcecc87c5f9e05d02161a4c0eaa7369c1983b309
truth check           PASS, max linear error=0.000432324718, max chroma error=0.000114263134
verify                24/24 tiles PASS, linear bound=0.0172497652, truth p95=0.000432324718
HTTP /api/preview     200, version 3, truth PASS
HTTP /api/exact       200, locked version 2, truth PASS
HTTP /api/verify      200, 24 tiles, per-pixel bounds length=6144
HTTP /frame.png?v=999 409 E_VER, versions=[999,1]
```

实际产物：

- `out/frame.png`
- `out/report.json`
- `out/verify.json`
- `checkpoints/current.json` 与当前 `snapshot-*`

## 边界说明

- 网格使用归一化屏幕坐标的正交投影和深度测试，重点是材质/光谱/并发/持久化链路，不是完整 PBRT。
- 间接光是有界、可配置次数的确定性反弹近似；误差上界已把反弹次数与衰减纳入。
- 真值是同一确定性物理模型的 1 nm、471 采样高精度参考结果，不是第三方 CIE 表或外部授权数据。
- 色域错误按颜色学语义检测负 RGB 通道；大于 1 的线性值允许作为 HDR，随后由曝光和色调映射压缩。
- 强制终止后的持久化保证以原子 `rename` 为前提；不会承诺未提交工作或未完成的最终图像存在。
