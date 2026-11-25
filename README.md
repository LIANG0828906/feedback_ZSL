# SpectralForge

SpectralForge 是一个仅使用 Go 标准库的本地光谱材质渲染管线与结果验证工具。它面向固定的美术材质样本，提供：

- 固定种子场景、逐顶点材质、逐波长光源、基函数定点系数、CMF、白点、目标色域和解析真值生成。
- 严格输入校验与稳定错误码：`E_SCHEMA`、`E_PARA`、`E_DECODE`、`E_GAMUT`、`E_QMIX`、`E_VER`、`E_SEAM` 等。
- 两种数值积分（梯形、中点）、可配置波长步数、白点归一和 sRGB 线性 RGB 输出。
- 固定点量化、逐样本量化误差报告、内容哈希与量化签名共同决定的材质缓存键。
- 固定屏幕块、多工作线程、每线程像素缓冲上限、有界块队列和等待原因统计。
- 原子串行块检查点、版本不匹配整体作废、暂停/恢复、全量/实时/恢复三条路径一致。
- 独立验证器：不导入 `internal/render`，从持久化块快照重新计算并定位首个不一致像素、块和字段。
- 本地页面：最新参数预览、精确版本锁定、恢复、独立校验和参数编辑。

## 环境

- Windows 10/11，Go 1.22 或更高版本；Linux/macOS 同样可运行。
- 不依赖 Docker、数据库、网络服务或第三方 Go 模块。

## Windows PowerShell 快速开始

```powershell
go build -o spectralforge.exe ./cmd/spectralforge
go test ./...

.\spectralforge.exe sample --dir sample --seed 20261125
.\spectralforge.exe validate --sample sample

.\spectralforge.exe render --sample sample --checkpoints checkpoints\exact --out render\exact --mode full
.\spectralforge.exe verify --sample sample --checkpoints checkpoints\exact --out render\exact

.\spectralforge.exe serve --sample sample --checkpoints checkpoints --out render --addr 127.0.0.1:8080
```

打开 `http://127.0.0.1:8080`。页面中的 `Exact (lock version)` 会锁定点击时的参数快照，`Resume exact` 只恢复已持久化且版本匹配的块。

## 三条路径

```powershell
# 一次性全量
.\spectralforge.exe render --sample sample --checkpoints checkpoints\full --out render\full --mode full

# 实时路径（增量提交，提交序仍为块索引序）
.\spectralforge.exe render --sample sample --checkpoints checkpoints\realtime --out render\realtime --mode realtime

# 暂停，再恢复
.\spectralforge.exe render --sample sample --checkpoints checkpoints\resume --out render\resume --mode realtime --stop-after 5
.\spectralforge.exe render --sample sample --checkpoints checkpoints\resume --out render\resume --mode resume
```

比较像素摘要与提交序列：

```powershell
.\spectralforge.exe compare `
  --a render\full --b render\resume `
  --a-checkpoints checkpoints\full --b-checkpoints checkpoints\resume
```

## 常用参数

- `--config <path>`：覆盖 `render-config.json`，用于编辑积分方式、波长步数、量化位深、曝光、色调曲线等。
- `--stop-after N`：提交 N 个新块后暂停。暂停报告不承诺完整帧。
- `--fail-tile N --fail-count 1`：注入一次可重试块失败；最终像素仍应与无失败路径一致。
- `--inject-gamut x:y:r:-0.5`：注入确定性的色域越界，用于验证 `E_GAMUT` 的像素和通道定位。
- `--snapshot-version <hash>`：声明期望参数版本；不一致返回包含两侧版本的 `E_VER`。

## 样本文件

`sample/` 中是可重新生成的固定种子样本：

| 文件 | 内容 |
| --- | --- |
| `scene.json` | 分辨率、网格、顶点位置/法线、反照率、粗糙度、金属度、基函数系数 |
| `spectral.json` | 波长、CMF、白点、色域矩阵、基函数采样、逐波长光源 |
| `truth.json` | 生成器解析构造的线性 RGB、峰值辐亮度、色度真值 |
| `render-config.json` | 渲染参数与量化参数 |
| `manifest.json` | 样本内容哈希与文件清单 |

`truth.json` 只由 `internal/sample` 在生成阶段计算，渲染器不回写真值。

## 输出

- `render/<name>/frame.json`：最终线性 RGB、显示 RGB、对象 ID、峰值辐亮度、块摘要和真值对照。
- `render/<name>/frame.png`：便于目视检查的显示图像。
- `render/<name>/blocks.json`：块摘要列表。
- `render/<name>/quantization.json`：逐样本量化前后值和误差统计。
- `render/<name>/verification.json`：独立验证报告。
- `<checkpoints>/manifest.json`、`tiles/*.json`、`commits.log`、`run-stats.json`：检查点和调度统计。

## 测试与自动化验收

```powershell
go test ./...
go test -race ./...
.\scripts\validate.ps1
```

Linux/macOS 可运行 `./scripts/validate.sh`。脚本执行构建、测试、样本生成、三条路径、恢复、独立校验、帧摘要比较，并输出 PNG 哈希。

更详细的算法、并发、检查点协议和需求映射见 [docs/DESIGN.md](docs/DESIGN.md)，实际运行结果见 [docs/VALIDATION.md](docs/VALIDATION.md)。

## 强制终止语义

每提交一个块都会原子写入其线性缓冲、场景内容哈希、采样版本、参数版本和材质缓存键。进程被强制终止时，只保证磁盘上已经完成且通过摘要校验的连续块前缀可恢复；不承诺生成完整退出报告或补齐未提交块。
