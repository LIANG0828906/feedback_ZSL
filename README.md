# MFX Forge

面向游戏与互动媒体素材的本地媒体资产管理与批量转码工作台。项目在全新工作区中从零构建，包含确定性样本生成器、逐变体转码核心、规模自适应策略、SQLite 检查点、三路径一致性比对、独立验证器和 `net/http` 诊断界面。

## 功能概览

- 工程加载与结构校验：源素材 SHA-256、PNG/WAV 实际规格、配方边界、编辑时间戳、目标变体、真值清单。
- 确定性转码：PNG/JPEG 解码归一、固定点面积重采样、8/10/16 位量化、4:4:4 / 4:2:2 / 4:2:0 色度采样、WAV PCM 重采样与位深转换、元数据规整、确定性 MFX 封装。
- 双规模路径：`fine` 逐像素精细路径；`adaptive` 分块精确输出并生成降采样预览摘要。两条路径共享同一像素与采样函数，输出逐字节一致。
- 变体状态机：`pending -> decoding -> encoding -> committed`，异常进入 `failed`，支持失败重试；已提交变体冻结产物与配方快照。
- SQLite 检查点：每个变体在一个事务中持久化产物 BLOB、摘要、真值、输入哈希、配方版本、算法版本与稳定提交序号。
- 三模式运行：`realtime` 处理未提交变体、`full` 整体作废后全量重算、`resume` 从匹配检查点续跑。
- 三路径比对：实时、全量、暂停后续跑三条路径逐变体比较产物指纹、元数据字段和提交序列，并输出 16×16 差异热力图数据。
- 独立验证器：只读取 SQLite 中的产物、主链路摘要和真值快照，独立解析 MFX，不导入转码包。
- 本地诊断台：项目/素材/变体、配方编辑、进度、路径依据、缓冲等待、失败重试、导出、验证和三路径结果。

## 环境

- Windows 10/11 64 位或兼容的 Linux/WSL 环境。
- Go 1.22 或以上。
- CGO 与 SQLite 动态库构建环境。Windows 建议 MinGW-w64 `gcc`，并设置：

```powershell
$env:CGO_ENABLED = "1"
go version
gcc --version
go mod tidy
go build -o bin\mediaforge.exe .\cmd\mediaforge
```

Linux/WSL：

```bash
export CGO_ENABLED=1
go version
gcc --version
go mod tidy
go build -o bin/mediaforge ./cmd/mediaforge
```

SQLite 驱动为 `github.com/mattn/go-sqlite3`，需要 CGO。无外部网络服务；样本生成、HTTP 服务和 SQLite 都在本机运行。

## 快速开始

```bash
# 1. 生成三组固定种子工程（约 20 MiB，含小/中/大规模判定样本）
./bin/mediaforge sample --out data --seed 42 --profile demo

# 2. 校验工程并查看自动规模判定
./bin/mediaforge validate --project data/open-world/project.json

# 3. 实时批处理，写入 SQLite 并导出 MFX
./bin/mediaforge run \
  --project data/open-world/project.json \
  --db work/open-world.sqlite \
  --out exports/open-world \
  --mode realtime --workers 2

# 4. 独立验证 SQLite 中的产物
./bin/mediaforge verify --db work/open-world.sqlite

# 5. 三路径比对
./bin/mediaforge compare-paths \
  --project data/open-world/project.json \
  --workers 2

# 6. 启动诊断台
./bin/mediaforge serve --data data --db work --addr 127.0.0.1:8787
```

浏览器打开 `http://127.0.0.1:8787`。界面只监听回环地址，不提供外部服务。

## 样本档位

`sample --profile` 支持：

- `small`：单工程，源素材数百 KiB，验证逐像素路径和完整元数据。
- `demo`：三工程，包含 `arcade-hud`、`boss-arena`、`open-world`，用于小/中/大规模自动取舍。
- `large`：单工程，源图 `8192×5120` + 备用图，素材总量达到数百 MiB 量级；适合手工压测，不建议在 CI 中运行。

固定种子相同则图片、WAV、工程清单和真值指纹完全相同。生成器会在写入真值前用共享确定性核心计算 MFX 指纹，再执行完整工程校验，防止清单与真值自相矛盾。

## 工程格式

每个工程目录包含：

```text
<project>/
  project.json
  assets/*.png
  assets/*.wav
```

`project.json` 的核心字段：

```json
{
  "schemaVersion": 1,
  "id": "open-world",
  "sources": [
    {
      "id": "image-base",
      "kind": "image",
      "path": "assets/image_base.png",
      "sha256": "...",
      "width": 2048,
      "height": 1280,
      "channels": 4,
      "metadata": {"title": "...", "author": "..."}
    }
  ],
  "baseRecipe": {
    "targetWidth": 1024,
    "targetHeight": 640,
    "imageChannels": 4,
    "imageBitDepth": 10,
    "chromaSampling": "4:2:0",
    "audioSampleRate": 48000,
    "audioBitDepth": 16,
    "metadataPolicy": "preserve"
  },
  "variants": [
    {"id": "v-main", "imageSource": "image-base", "audioSource": "audio-base"}
  ],
  "operations": [
    {"seq": 1, "time": 100, "type": "replace_source",
      "variant": "v-alt", "media": "image", "source": "image-alt"},
    {"seq": 2, "time": 200, "type": "set_recipe",
      "field": "imageBitDepth", "value": 10},
    {"seq": 3, "time": 300, "type": "add_variant",
      "newVariant": {"id": "v-preview", "imageSource": "image-base", "audioSource": "audio-base"}},
    {"seq": 4, "time": 400, "type": "remove_variant", "variant": "v-draft"}
  ],
  "truth": {"variants": []},
  "budget": {
    "maxWorkers": 2,
    "memoryBytes": 268435456,
    "perVariantMillis": 8000,
    "maxPixelsPerWorker": 4000000,
    "maxSamplesPerWorker": 4000000,
    "fineMaxPixels": 4000000,
    "fineMaxSamples": 8000000,
    "fineMaxVariants": 4,
    "adaptiveBlockPixels": 2000000,
    "maxDownsample": 8
  },
  "consistency": {
    "maxPixelValueDelta": 0,
    "maxSampleValueDelta": 0,
    "requireByteIdentical": true
  }
}
```

支持的操作：

- `replace_source`：替换目标变体的图像或音频源。
- `set_recipe`：修改全局或指定变体的配方字段。
- `add_variant`：增加输出变体。
- `remove_variant`：删除输出变体。

操作 `seq` 与逻辑时间 `time` 必须严格递增；目标变体必须在操作发生时存在。

## MFX 产物

每个变体输出一个确定性二进制包：

```text
8 bytes   "MFXPKG01"
4 bytes   big-endian header length
JSON      canonical MFX header
payload   image samples: uint16 little-endian, W x H x channels
payload   audio samples: uint32 little-endian, samples x channels
```

Header 包含：

- `width`、`height`、`pixelCount`
- `imageChannels`、`imageBitDepth`、`chromaSampling`
- `audioSampleRate`、`audioBitDepth`、`audioChannels`、`audioSamples`
- `metadata` 与排序后的 `metadataFields`
- `algorithmVersion`

完整文件 SHA-256 即变体内容指纹。包内不含墙钟时间、线程 ID、随机数或完成顺序字段。

## 规模自适应

批量判定依据：

- 全部变体的源像素总数；
- 音频采样点总数；
- 变体数；
- 估算工作集与内存预算；
- 单变体估算毫秒与 `perVariantMillis` 时间预算；
- 精细阈值和分块阈值。

小规模满足全部精细阈值时固定选择 `fine`。否则选择 `adaptive`，块高受 `adaptiveBlockPixels` 与 `maxPixelsPerWorker` 约束；`4:2:0` 时块高对齐偶数行，保证色度块不会在分块边界重新配对。自适应路径还会生成采样预览摘要，并记录预览尺寸、倍率和 SHA-256。

自适应路径输出仍走同一面积重采样、色度采样、位深量化与音频重采样函数。启用 `requireByteIdentical` 时，运行器会额外执行精细路径并调用 `CompareProducts`；像素值、采样值或整包字节超差返回 `E_SCALE`，并带 `field/actual/expected`。

## 状态与检查点

合法状态迁移：

```text
pending -> decoding -> encoding -> committed
decoding/encoding -> failed
failed -> pending -> decoding
decoding/encoding -> pending        # 用户暂停或取消
```

`committed` 是冻结终态。非法迁移返回 `E_STATE`，事务会保持原状态不变。

SQLite 主要表：

- `variants`：状态、路径决策、配方快照、输入哈希、配方版本、算法版本、产物摘要、真值、产物 BLOB、提交序号。
- `invalidations`：检查点整体作废原因。
- `stalls`：像素/采样缓冲等待记录。
- `meta`：工程指纹、工程 ID、算法版本。

每次提交使用单写事务，`SetMaxOpenConns(1)` 与进程内互斥保证写入串行。工作线程可以并行解码/编码，但协调器严格按变体序号提交，因此线程完成顺序不会改变提交序列。

重启续跑时：

1. 比较工程指纹、工程 ID、算法版本。
2. 比较每个已提交变体的输入哈希、配方版本和算法版本。
3. 任一不匹配则整体删除产物与快照，全部变体回到 `pending`。
4. `decoding`/`encoding` 中间态统一回滚为 `pending`。

不会只复用一部分旧产物。强制终止不承诺产生退出报告；只有已提交且版本匹配的变体可恢复。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_DECODE` | 清单缺失/损坏、素材损坏、哈希/尺寸/通道/音频参数不一致、操作目标不存在、时间戳非法 |
| `E_PARA` | 配方或操作参数越界，错误对象包含字段名 |
| `E_SCALE` | 精细与自适应路径超出一致性约束 |
| `E_STATE` | 非法状态迁移、提交后修改、无运行任务暂停 |
| `E_DIFF` | 真值不一致、三路径不一致、独立验证发现数据差异 |
| `E_VERIFY` | 验证器自身无法读取/解析持久化产物或缺少检查点 |
| `E_IO` / `E_BUDGET` / `E_CANCEL` / `E_INTERNAL` | 本地 I/O、资源预算、取消、内部异常 |

CLI 将错误以 JSON 输出到 stderr，例如：

```json
{
  "ok": false,
  "error": {
    "code": "E_PARA",
    "message": "must be 8, 10 or 16",
    "field": "baseRecipe.imageBitDepth"
  }
}
```

## CLI

```text
mediaforge sample        --out DIR [--seed N] [--profile small|demo|large]
mediaforge validate      --project PROJECT.json
mediaforge run           --project PROJECT.json --db CHECKPOINT.sqlite
                         [--out DIR] [--mode realtime|full|resume]
                         [--workers N] [--pause-after N] [--force-path fine|adaptive]
                         [--only VARIANT]
mediaforge serve         [--data DIR] [--db DIR] [--addr 127.0.0.1:8787]
mediaforge verify        --db CHECKPOINT.sqlite
mediaforge compare-paths --project PROJECT.json [--workers N] [--pause-after N]
mediaforge export        --db CHECKPOINT.sqlite --variant ID --out FILE
mediaforge state         --db CHECKPOINT.sqlite
```

模式语义：

- `realtime`：初始化或继续现有检查点，只处理 `pending` 变体；已提交结果保持冻结。
- `full`：整体作废快照并从源素材重算全部变体。
- `resume`：只使用版本匹配的已提交检查点，回滚中断活动态后继续。

## 诊断 HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 诊断界面 |
| GET | `/api/projects` | 工程列表 |
| POST | `/api/select` | `{"path":"..."}` 切换工程 |
| GET | `/api/state` | 项目、素材、变体、进度、路径、热力图、事件 |
| POST | `/api/run` | `{"mode":"...","workers":2,"pauseAfter":0}` |
| POST | `/api/pause` | 暂停当前批处理 |
| POST | `/api/recipe` | 修改未提交变体配方并重算真值 |
| POST | `/api/retry` | 单变体重试 |
| GET | `/api/export?variant=ID` | 下载已提交 MFX |
| GET | `/api/source?id=ID` | 本地回环预览源素材 |
| POST | `/api/verify` | 启动独立验证 |
| POST | `/api/compare-paths` | 启动三路径比对 |

## 测试

```bash
go test ./...
go vet ./...
go test -race ./internal/codec ./internal/state ./internal/integration
```

测试覆盖：

- 精细/自适应重复转码逐字节一致；
- 4:2:0 分块边界与预览摘要；
- `E_SCALE` 超差；
- `E_PARA`、`E_DECODE` 字段定位；
- 状态机合法/非法迁移；
- 实时全量重转、暂停、关闭数据库、重启续跑产物一致；
- 检查点版本篡改后整体失效；
- 独立验证器发现真值差异与自身解析异常；
- 三路径比对及零差异热力图；
- HTTP 页面、批处理、冻结提交拒绝与验证流程。

实际验收结果记录在 `artifacts/validation.json`。
