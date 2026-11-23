# ForensicLab

面向游戏贴图与互动媒体素材的本地图像取证与完整性分析工作台。项目由 Go 取证引擎、SQLite 版本化检查点、Wails 桌面面板和确定性样本生成器组成，不依赖网络服务、账号或容器环境。

## 交付内容

- 确定性样本生成器：固定种子生成单通道/三通道 PNG、原始基准图、篡改掩码和 JSON 真值清单。
- 块级取证链：内容哈希、量化指纹、噪声残差方差、边缘方向一致性、局部对比度和篡改分数。
- 精确失效传播：`hash -> analysis(stats) -> analysis(verdict) -> summary -> heatmap` 分块依赖图。
- 增量与三路径验证：增量编辑、空缓存全量、单线程串行三路逐像素/逐块/提交序列比对。
- SQLite 持久化：每块单事务提交，参数/算法/内容版本不匹配时整快照作废。
- 可暂停、取消、强杀后续跑的 Wails 桌面面板。
- 一键复现实验：`scripts/acceptance.sh`。

## 快速开始

```bash
# 1. 确定性样本
go run ./cmd/forensiclab generate --dir artifacts/samples

# 2. 全量/断点续跑 CLI
go run ./cmd/forensiclab run \
  --manifest artifacts/samples/manifest.json \
  --image game_tex_rgb_160x112 \
  --db artifacts/workbench.sqlite \
  --threshold 0.45 --block 16 --window 3

# 3. 同一进程内执行增量阈值编辑，并导出报告
go run ./cmd/forensiclab sequence \
  --manifest artifacts/samples/manifest.json \
  --image game_tex_rgb_160x112 \
  --db artifacts/incremental.sqlite \
  --thresholds 0.70,0.45,0.30 \
  --report artifacts/reports

# 4. 三路径自动比对
go run ./cmd/forensiclab verify \
  --manifest artifacts/samples/manifest.json \
  --image game_tex_rgb_160x112 \
  --workdir artifacts/verification \
  --thresholds 0.70,0.45 \
  --report artifacts/reports

# 5. 完整验收（单元/集成/race、增量、三路径、强杀恢复）
./scripts/acceptance.sh
```

CLI 所有结果以 JSON 输出，错误包含稳定错误码和定位字段。

## 桌面应用

Wails 面板为离线静态前端，不引用 CDN。构建需要 GTK 3 和 WebKit2GTK 开发包：

```bash
sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.1-dev pkg-config
npm --prefix frontend run build
wails build -tags "wails,webkit2_41" -s
./build/bin/forensiclab
```

也可使用：

```bash
./scripts/build-desktop.sh
```

本环境没有系统写权限，无法直接安装开发包；构建时使用了工作区私有 sysroot，并成功生成 12 MB ELF 桌面程序。该程序在只读 `bubblewrap` 路径映射下启动，窗口事件循环稳定运行，超时退出前输出 `Ctrl+C detected. Shutting down...`。`scripts/build-desktop.sh` 同时支持系统依赖和 `FORENSICLAB_SYSROOT` 私有前缀。

面板功能：

- 原图与热力图叠加、分块网格和状态矩阵。
- 阈值、窗口、分块、哈希/指纹参数、并行度、像素缓冲和队列容量调整。
- 增量运行、暂停、继续、取消、选中块重试、清空检查点。
- 失效传播清单、缓存复用清单、资源阻塞诊断。
- 三路径逐像素比对、内容哈希核验和 JSON/CSV/PNG 报告导出。

## 工程结构

```text
.
├── app.go                       # Wails 绑定与桌面会话状态
├── main.go                      # Wails 入口（build tag: wails）
├── main_stub.go                 # 无桌面依赖时的 CLI 提示入口
├── frontend/dist/               # 可嵌入的离线页面
├── internal/forensic/
│   ├── sample.go                # 样本生成、清单/图像/掩码加载与校验
│   ├── hash.go                  # 块内容哈希、量化指纹、E_HASH 定位
│   ├── analysis.go              # 噪声/边缘/对比度统计与真值评估
│   ├── deps.go                  # 缓存键、阶段标识、依赖图
│   ├── engine.go                # 调度、失效、暂停取消、热力图
│   ├── store.go                 # modernc.org/sqlite 检查点
│   ├── verify.go                # 三路径逐像素/逐块比对
│   └── report.go                # 取证报告和 CSV
├── cmd/forensiclab/             # 无 GUI 验收 CLI
├── scripts/                     # 构建与验收脚本
└── artifacts/
    ├── samples/                 # 固定种子样本与真值
    ├── reports/                 # 最近一次取证报告
    └── acceptance/              # 验收日志与原始 JSON
```

## 样本契约

清单格式为 `forensiclab.samples/v1`，每个样本声明：

- `width`、`height`、`channels`、`seed`
- `primitiveCount` 和带边界坐标的 `primitives`
- 篡改区域 `tamperRegions`
- `maskWidth`、`maskHeight`、`maskTolerance`
- 原图、基准图、掩码路径及 SHA-256

内置四个样本：

| 图像 | 分辨率 | 通道 | 篡改区域 |
|---|---:|---:|---:|
| `game_tex_rgb_160x112` | 160×112 | 3 | 2 |
| `game_tex_gray_96x96` | 96×96 | 1 | 2 |
| `ui_atlas_rgb_128x80` | 128×80 | 3 | 1 |
| `mask_gray_64x64` | 64×64 | 1 | 1 |

样本内容只由自定义 SplitMix64 序列和固定参数生成，清单不含时间戳。篡改区域叠加确定性高频噪声，使噪声残差统计可稳定区分并保证阈值降低时判定集合单调不减。

## 错误契约

| 错误码 | 触发场景 | 定位信息 |
|---|---|---|
| `E_PARA` | 分块、窗口、阈值、算法、缓冲等参数非法 | `field` |
| `E_DECODE` | 清单缺失/损坏、PNG 或掩码无法解码、校验和不符 | 图像标识与原因 |
| `E_META` | 声明尺寸/通道/图元数量/掩码尺寸冲突 | `imageId`、`field` |
| `E_HASH` | 持久化或期望内容哈希与当前块不一致 | 首个不一致 `block`、`stage=hash` |
| `E_DIVERGE` | 三条路径热力图/摘要/提交序列不一致 | 首个像素 `x,y`、`block`、`stage` |

## 分析定义

每个块只读取自身像素，窗口在块边界内收缩，因此结果不受线程数、调度顺序或缓存命中影响。

- 内容摘要：块尺寸/通道头 + 行优先交错像素的 SHA-256，用于快照内容校验。
- 内容哈希：可配置 `sha256`、`sha512`、`sha1`、`fnv64a`，输入为同一规范块内容。
- 指纹：量化亮度网格 `gN:<nibble...>`，只依赖像素和 `fingerprintGrid`。
- 噪声方差：像素与块内可配置窗口均值的残差平方均值。
- 残差覆盖率：绝对残差大于量化门限（9）的像素占比，用于区分持续噪声与单条图元边缘。
- 边缘方向一致性：Sobel 梯度在 8 个方向桶中的最大能量占比。
- 局部对比度：块亮度标准差 / 255。
- 篡改分数：`0.72*噪声项 + 0.16*边缘不一致项 + 0.12*对比度项`，其中噪声项为 `min(RMS/32,1) * min(覆盖率/0.55,1)`；分数大于等于阈值判为篡改。

默认阈值 `0.45` 时，四个样本的块级真值吻合率均高于清单容差 `0.08`；RGB 主样本为 `1.0000`。

## 缓存键与失效策略

缓存键逐级包含上游键：

```text
hashKey    = H("hash", contentDigest, hashAlgorithm, fingerprintGrid, algoVersion)
statsKey   = H("analysis/stats", hashKey, window, algoVersion)
verdictKey = H("analysis/verdict", statsKey, threshold, algoVersion)
summaryKey = H("summary", verdictKey, algoVersion)
heatmapKey = H(final, all summaryKeys)
```

失效规则：

- 内容变化：整个图像快照作废，全量重算。
- 分块尺寸变化：块布局变化，整个快照作废。
- 哈希算法/指纹粒度变化：`hash`、`analysis`、`summary`、`heatmap` 失效。
- 窗口变化：复用哈希，失效所有块分析与下游汇总。
- 阈值变化：统计量不依赖阈值，复用哈希与统计，只重算判定翻转块的分析/汇总；未翻转块继续复用。
- 选中块重试：仅指定块分析和汇总失效，其它块完全复用。
- 并行度、队列容量、像素缓冲变化：不影响确定性结果，不改变缓存键。

工作线程并行计算，但提交循环严格按块号升序执行。SQLite 使用单连接、写互斥和每块事务，因此提交账本顺序与单线程路径相同。像素缓冲采用加权容量限制，等待时记录 `pixel_buffer_limit_blocked` 或 `queue_capacity_blocked`，不会静默丢块。

## 检查点与恢复

SQLite 表：

- `schema_meta`：模式版本。
- `snapshots`：图像、参数版本、算法版本、块尺寸、完成状态、热力图与摘要键。
- `blocks`：内容摘要/哈希/指纹、统计量、判定、各阶段缓存键、参数/算法版本、提交序号。
- `commits`：追加式提交账本。

每次块提交在同一个事务中更新 `blocks` 并追加 `commits`。启动恢复规则：

1. 内容摘要、参数版本、算法版本、布局任一不匹配，删除该图像全部块状态并全量重算。
2. 同版本且未完成时，只复用已持久化块，缺失块重算。
3. 不使用缺失块推断真值或统计量。
4. 阈值增量在全部块处理完成后，以单事务提升未翻转块的版本元数据；中途终止会形成混合版本，重启时按规则整体作废。

## 三路径比对

`VerifyThreePaths` 在三个独立数据库上运行：

1. 增量路径：按编辑序列在同一引擎中执行。
2. 全量路径：空缓存直接使用最终参数。
3. 串行路径：空缓存、重放同一编辑序列、`workerCount=1`、`queueCapacity=1`。

比对内容：

- 热力图 RGBA 逐字节逐像素。
- 每块内容摘要、哈希、指纹、统计量、判定和汇总键。
- 最终块提交序列（块号升序）。

失败时返回 `E_DIVERGE`，包含首个差异像素坐标、所属块号和分析/汇总阶段。

## 已执行验收

环境：Go `1.22.12`、Wails CLI `2.10.2`、Node `20.18.1`、`modernc.org/sqlite v1.31.1`、Linux amd64。

自动化：

```text
go test ./... -count=1                              PASS
go test -race ./internal/forensic -count=1          PASS
node --check frontend/dist/app.js                   PASS
npm --prefix frontend run build                     PASS
```

确定性样本上的增量阈值编辑：

| 阈值 | 分块 | 篡改 | 复用 | 重算 | 失效记录 | 真值吻合 |
|---:|---:|---:|---:|---:|---:|---:|
| 0.70 | 70 | 2 | 0 | 70 | 211 | 0.9143 |
| 0.45 | 70 | 8 | 64 | 6 | 13 | 1.0000 |
| 0.30 | 70 | 9 | 69 | 1 | 3 | 0.9857 |

三路径最终参数 `threshold=0.45, block=16, window=3`：

```text
heatmapEqual  = true
summaryEqual  = true
sequenceEqual = true
SHA-256       = e5eb65aed2d5f4d0677815b363dea4af40f75e0659b1afa472414caadc3566aa
commit length = 70
```

强制终止：

```text
第 5 个块原子提交后退出                  exit=99
重启复用已持久化且版本匹配块             5
重算缺失块                               65
最终完成                                 true
提交账本长度                             70
```

同库改用 `window=5` 后：

```text
snapshotResetReason = params_version_mismatch
reusedBlocks        = 0
recomputedBlocks    = 70
```

完整原始数据见 `artifacts/acceptance/summary.json`、`sequence.json`、`threeway.json`、`resume-run.json`；报告见 `artifacts/reports/`。

## 设计边界

- 分析窗口限制在块内，避免块边界依赖，换来严格分块级缓存和确定性。
- 阈值使用“分数大于等于阈值”判定。数值降低表示更敏感；由高到低调整时篡改集合单调增加。
- 报告包含运行时间；缓存版本和热力图本身不受运行时间影响。
- 热力图按块汇总分数着色，因此“逐像素一致”验证的是相同分块最终热力图的每个像素值；块内统计不伪造像素级局部判定。
