# TexLab · 位深与通道转换工作台

TexLab 是面向游戏贴图与互动媒体素材的确定性位深/通道转换工作台。应用由 Go 核心引擎、Wails 桌面壳和零依赖静态前端组成；同一套模型与服务也提供本地 HTTP 无头模式，便于在无桌面依赖的环境中复现实验与验收。

核心能力：

- 固定种子生成 8/16 位、RGBA/BGRA、384 到 2048 像素宽的本地样本、逐条目逐像素真值和转换计划。
- 对通道置换、通道增减、位深量化、alpha 预乘/非预乘进行逐像素确定性处理。
- 基于输入区域内容哈希、条目序号、参数哈希和上游缓存键构建阶段缓存。
- 编辑参数或重排条目后，按实际覆盖分段精确失效，并输出条目/分段/区域/原因/复用状态清单。
- 分段状态机、冻结参数快照、版本混用检测、原子串行检查点和全量恢复校验。
- 增量、断点恢复、全量重算三路径逐像素一致；串行与并行结果及提交顺序一致。
- 独立验证器只读持久化分段、计划和真值，重新拼装最终图像、逐条目摘要并进行色块/alpha 统计。
- 可重复生成的验证报告，同一计划与参数下逐字节一致。

## 环境与版本

目标桌面环境为 Windows 10/11，要求：

- Go 1.22 或更高版本，`go env GOPATH` 可写。
- Wails CLI 2.x；本项目固定使用 `github.com/wailsapp/wails/v2 v2.10.2`。
- Windows WebView2 Runtime。
- Node.js 18+ 为可选项。当前前端是纯 HTML/CSS/JS，无 npm 依赖，因此不安装 Node 也能构建和运行。

本次实际验证环境：

| 组件 | 实际值 |
|---|---|
| OS | Ubuntu 26.04（WSL/Linux 验证环境） |
| Go | `go1.22.12 linux/amd64` |
| Wails CLI | `v2.10.2` |
| Node.js | 未安装；静态前端不依赖 Node |
| 样本 | 本地程序生成，不访问网络 |

当前 Linux 验证容器缺少 `pkg-config`、GTK3 和 WebKit2GTK，因此 Linux 原生窗口链接不可用。不过 Wails CLI 的 Windows 目标构建已实际成功：

```text
platform: windows/amd64
output:   build/bin/texlab.exe
format:   PE32+ GUI, x86-64
```

目标 Windows 构建按下面命令执行。无头模式也已在当前容器内完整运行。

## 快速开始

### 1. 安装与检查

```powershell
go version
go env GOPATH
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails doctor
```

确认 `go env GOPATH` 指向可写目录，并把 `$(go env GOPATH)/bin` 或 `%USERPROFILE%\go\bin` 加入 `PATH`。

### 2. 生成确定性样本

```powershell
go run ./cmd/texlab generate -samples samples
go run ./cmd/texlab list -samples samples
```

样本目录包含：

```text
samples/<name>/
  source.gtex        # 自描述源图，含 SHA-256
  plan.json          # 有序转换条目、区域、分段网格
  truth.json         # 每条目逐像素参考、色块值、alpha 台阶
  manifest.json      # 种子、源图头、计划/真值指纹、完整生成规格
```

### 3. 构建 Wails 桌面应用

桌面入口使用 `desktop` 构建标签；前端是预构建静态资源，使用 `-s` 跳过前端构建命令。项目直接调用 Wails 注入的 `window.go`，不需要生成 TypeScript/JS 包装模块，因此使用 `-skipbindings`：

```powershell
wails build -tags desktop -s -skipbindings
.\build\bin\texlab.exe
```

当前工作区已包含一次成功构建的 `build/bin/texlab.exe`；重新构建会原子替换该文件。

开发模式：

```powershell
wails dev -tags desktop -s -skipbindings
```

Windows 构建所需图标位于 `build/appicon.png` 与 `build/windows/icon.ico`。

### 4. 无头交互模式

没有桌面依赖时可运行同一工作台：

```powershell
go run . -samples samples -data .texlab\data -addr 127.0.0.1:34115
# 打开 http://127.0.0.1:34115
```

也可以使用 CLI：

```powershell
go run ./cmd/texlab serve -samples samples -data .texlab\data
```

无头页面与 Wails 页面共用同一套服务、编辑、增量重算、矩阵、热力图、验证和导出逻辑。

## 确定性样本

| 样本 | 分辨率 | 位深 | 源布局 | 种子 | 条目 |
|---|---:|---:|---|---:|---:|
| `aurora_8_rgba` | 384×216 | 8 | RGBA | 1101 | 6 |
| `forge_16_bgra` | 1024×576 | 16 | BGRA | 2202 | 6 |
| `atlas_8_bgra` | 2048×384 | 8 | BGRA | 3303 | 6 |

每个样本包含已知色块、水平/对角渐变、棋盘带、透明色块和 16 档 alpha 台阶。默认计划依次包含：

1. 8→16 位、`RGBA→BGRA`、全图。
2. 16→8 位、alpha 预乘、`floor` 量化、全图。
3. 8→16 位、alpha 非预乘、`ceil` 量化、全图。
4. 16→16 位、区域 `RG` 交换，只覆盖 `top_band` 与 `alpha_steps`。
5. 16 位 `RGBA→BGR`、丢弃 alpha，全图。
6. 8 位 `BGR + 常量 A=255`，全图。

区域按 4×3 分段网格切分，因此默认编辑的精确失效范围为 6 个实际覆盖分段及其下游条目。

## 图像与计划语义

### `.gtex`

`.gtex` 是本地确定性二进制格式：

- 魔数 `GTEX0001`。
- 宽、高、位深、布局、通道数头信息。
- 像素按声明布局存储；BGRA 文件按 B/G/R/A 字节序落盘，解码后恢复语义 R/G/B/A。
- 尾部 SHA-256 校验；长度、布局、位深或校验不符返回 `E_DECODE`。

### 通道置换

语义通道固定为 `R=0, G=1, B=2, A=3`。`permutation` 的长度等于 `target_channels`，每个目标槽声明来源通道索引：

- RGBA→BGRA：`[2,1,0,3]`。
- 4→3 丢 alpha：`[2,1,0]`，`drop=[3]`。
- 3→4 补 alpha：`[2,1,0,-1]`，并为目标槽 3 声明填充来源。

填充来源支持 `constant`、`zero`、`one`、`alpha`、`channel`。减少通道时，丢弃集合必须恰好等于未被置换使用的源通道集合。

### 位深与量化

- 8→16 位使用精确比例 `65535/255`。
- 16→8 位按 `floor`、`round` 或 `ceil` 量化。
- 目标尺寸严格等于输入尺寸，不会重采样。

### Alpha 约定

条目声明目标 alpha 约定：

- `straight → premultiplied`：`round(c*a/max)`。
- `premultiplied → straight`：`round(c*max/a)`。
- alpha 为 0 时保留直通 RGB 载荷，使零 alpha 台阶也可逆；这是 TexLab alpha 约定的一部分。

预乘/非预乘往返误差上界由 `AlphaStepBound(alpha, depth)` 给出，测试覆盖 8/16 位和多个 alpha 档位。

## 缓存、失效与状态

### 缓存键

```text
cache_key = SHA256(
  "texlab-cache-key-v1",
  input_region_content_hash,
  entry_index,
  entry_parameter_hash,
  upstream_cache_key
)
```

`entry_parameter_hash` 覆盖目标位深、通道数、置换、填充、丢弃、alpha、量化和区域声明。分段缓存还保存完整输入哈希、区域哈希、覆盖签名、输出内容哈希和参数快照。

### 精确失效

执行时对每个“条目 × 分段”：

- 参数、输入内容、覆盖和上游都未变：复用缓存，不调用转换。
- 条目区域未覆盖该分段：直通复制上游数据，不执行通道转换。
- 条目参数改变：只重算其实际覆盖分段。
- 下游条目只有在其覆盖分段收到变化输入时才重算。
- 条目重排：未变化的阶段可在新缓存键下复用数据；真正改变输入的下游覆盖才重算。

### 分段状态机

合法迁移：

```text
pending    -> processing
failed     -> processing
processing -> committed
processing -> failed
```

`committed` 参数快照冻结。同一版本尝试重开已提交单元返回 `E_STATE` 且不改变状态；跨版本混用返回 `E_VERSION`，错误包含分段和两侧版本。

## 检查点与恢复

每个分段提交时，串行写入该分段所有条目的：

- 输出像素、位深/通道/布局/alpha。
- 缓存键、输入/区域/输出内容哈希。
- 计划指纹、参数版本、算法版本。
- 条目参数快照、复用/失效状态。

写入使用互斥锁、同目录临时文件、`fsync`、原子重命名和目录 `fsync`，不会交错。加载时会校验：

- 元数据、计划/源图/参数/算法版本一致。
- 分段序号连续。
- 条目顺序和参数快照一致。
- 上游缓存键链、输入内容哈希链一致。
- 每个阶段及最终图像的输出哈希一致。

任一校验失败都会整体删除快照，禁止部分混用旧结果。编辑计划后，计划版本持久化到数据目录，重启后继续当前版本。

## 独立验证器

验证器不调用转换引擎，只从以下输入工作：

- 当前计划文件。
- 当前真值文件及真值指纹。
- 持久化分段结果。
- 主链路运行记录。

它自行解码 zlib 像素、按分段拼装最终图像、重算逐条目内容摘要，并与主记录和真值逐像素比较。首次差异返回 `E_DIFF`，包含全局像素坐标、条目序号、条目 ID、分段编号、字段名、期望值和实测值。

报告包含：

- 计划/源图/真值指纹、算法版本。
- 逐条目摘要和最终分段摘要。
- 已知色块期望/实测/最大通道偏差。
- alpha 台阶期望/实测。
- 提交序列和最终摘要。

报告不写入时间戳、临时路径或随机内容；同计划同参数下逐字节一致。

## 异常码

| 码 | 含义 |
|---|---|
| `E_DECODE` | 源图损坏、长度/布局/校验失败 |
| `E_PLAN` | 计划字段缺失/非法，返回条目序号与字段名 |
| `E_PARA` | 单条目参数越界，返回字段名 |
| `E_STATE` | 非法状态迁移，原状态保持不变 |
| `E_VERSION` | 已提交快照与当前版本混用 |
| `E_DIFF` | 像素/摘要/提交序列不一致，返回首个差异位置 |
| `E_IO` | 本地文件读写失败 |
| `E_CONCURRENCY` | 并发/资源参数非法 |
| `E_CHECKPOINT` | 检查点持久化或加载失败 |

## 并发与资源边界

- 工作线程数可通过 CLI `-workers` 或服务构造参数限制，默认 4，上限 64。
- 每线程像素 scratch 上限由 `-pixels` 控制，默认 262144。
- 分段任务进入无缓冲工作队列；缓冲不足时记录 `buffer_banded`，工作仍按确定性顺序完成，不会静默丢弃。
- 分段结果可并行计算，但按分段序号串行提交、写检查点并记录提交序列。
- 串行与并行路径对同一计划/编辑序列必须产出相同像素、阶段摘要和提交序列；验收 CLI 会显式比较。

## 验收与实测结果

### 自动测试

```powershell
go test ./...
go vet ./...
```

覆盖：

- `.gtex` 8/16 位、RGBA/BGRA 往返与损坏检测。
- 通道置换、增/减通道、三种量化方向、alpha 往返误差。
- 计划字段/填充/丢弃/区域校验与异常上下文。
- 状态机合法/非法迁移和版本冻结。
- 增量 vs 全量、并行 vs 串行、断点恢复 vs 不间断。
- 条目重排后的增量/全量一致性。
- 计划编辑后的动态真值验证、计划跨重启持久化、导出。
- 验证报告逐字节一致。

### 三样本完整验收

```powershell
go run ./cmd/texlab acceptance -samples samples -out artifacts\acceptance
```

实际结果：三组样本全部 `PASS`。

| 样本 | 验证 | 报告 SHA-256 | 最终摘要 |
|---|---|---|---|
| `aurora_8_rgba` | `MATCH` | `34db88293947d2ef71dff62d0dca35e584b59375f99c83734a449e4727c3ee89` | `b43e9412fcc75db2967b8eedab6ee93f92cdfcfbc4b3717f1c11c7e5fe1117e4` |
| `forge_16_bgra` | `MATCH` | `2cc708c620278a09a4298c9d2fa43906f3b99f9be1663c2345271e8c060ef589` | `20b821f92d7a103cf1b27492590a7ba4cdf11a9dc3dbac56f93eee429d08c899` |
| `atlas_8_bgra` | `MATCH` | `9bfb05afa3ebca3784ef41e98ceed4ddf96304bab3e8a06c24b9eb40c0fc6f10` | `888addced442a2c68ae9457ca77778f8b17620abacd63d14a4cd97f739393327` |

每样本验收同时核对：

- 报告两次生成逐字节一致：`true`。
- 增量重算 == 全量重算：`true`。
- 条目重排增量 == 全量：`true`。
- 断点恢复 == 未中断路径：`true`。
- 串行 == 并行：`true`。
- 编辑区域实际覆盖 6 个分段；条目 `regional_swap` 与下游 `drop_alpha` 共 18 个单元格失效，54 个单元格复用。
- 提交序列哈希均为 `e8238a69dd2b4b90939a921492997b6f309e87cb2b36721f68fca092e5a1be14`。

验收报告位于：

```text
artifacts/acceptance/acceptance-report.json
artifacts/acceptance/aurora_8_rgba-verification-report.json
artifacts/acceptance/forge_16_bgra-verification-report.json
artifacts/acceptance/atlas_8_bgra-verification-report.json
```

## CLI 速查

```powershell
go run ./cmd/texlab generate   -samples samples
go run ./cmd/texlab list       -samples samples
go run ./cmd/texlab run        -samples samples -data .texlab\data -name aurora_8_rgba
go run ./cmd/texlab verify     -samples samples -data .texlab\data -name aurora_8_rgba
go run ./cmd/texlab acceptance -samples samples -out artifacts\acceptance
go run ./cmd/texlab serve      -samples samples -data .texlab\data
go run ./cmd/texlab doctor
```

## 目录结构

```text
app.go                     Wails 绑定方法
main_desktop.go            Wails 桌面入口（build tag: desktop）
main_headless.go           本地 HTTP 无头入口
frontend/dist/             零依赖静态工作台
internal/model/            计划、图像、分段、哈希、差异
internal/gtex/             GTEX 编解码与完整性校验
internal/plan/             计划加载与字段校验
internal/convert/          通道/位深/alpha 确定性转换
internal/state/            分段状态机与冻结快照
internal/engine/           并发调度、缓存键、失效、路径比较
internal/checkpoint/       原子串行持久化与整体恢复校验
internal/runrecord/        主链路运行记录与摘要
internal/verify/           独立结果验证器与稳定报告
internal/sample/           固定种子样本、计划和独立参考真值
internal/truth/            真值编解码/指纹
internal/preview/          PNG 预览与差异热力图
internal/workbench/        工作台服务、编辑、导出、动态真值
cmd/texlab/                生成/运行/验证/验收 CLI
cmd/genicon/               可重复生成应用图标
```

## 设计说明

- 所有像素比较使用内部 16 位槽位；8 位源值保持 0..255，仅在目标位深为 16 时按确定性比例扩展。
- 部分区域操作保持未覆盖像素原值，仅覆盖像素执行该条目转换；默认样本的区域与分段边界对齐。
- 编辑后会为新计划重新生成独立参考真值，因此“增量、全量、恢复”之外还能持续运行独立验证。
- 验证器与主转换链共享纯哈希/数据结构，但不调用 `internal/convert` 或调度器。
- 不使用 Docker、外部图像或未授权网络资源；样本生成、持久化和验证均为本地操作。
