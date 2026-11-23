# OCR Workbench

面向游戏截图与互动媒体素材的本地 OCR 文字检测识别桌面工作台。项目从零构建，包含确定性样本生成器、像素级文本行检测、字符切分与模板匹配识别、双层状态机、SQLite 原子检查点、断点恢复、三路径一致性比对、Wails 桌面入口和同核心的浏览器实验台。

所有样本、字形模板和测试素材均在本地生成，不读取网络或外部图片。真值只来自生成器的构造记录；检测与识别链路从不读取真值，真值仅用于结构校验、质量统计与验收。

## 交付内容

- 5 个固定种子合成样本：不同分辨率、RGBA/灰度、单行/多行、倾斜、低对比度、稀疏/密集区域。
- 每个文字区域附带四点包围框、文本、字符数、平均笔画宽、构造行号。
- 灰度归一化、Otsu 二值化、梯度加权连通域、游程笔画宽度估计、瓦片核心归属、重叠度与笔画一致性合并、阅读顺序校验。
- 自适应去倾斜、列投影字符切分、确定性 `bitmap5x7-v1` 模板库匹配、逐字符与逐行置信度、显式空行。
- 瓦片与文本行两层四态状态机，合法迁移校验，失败单元重试，已提交结果冻结。
- SQLite WAL 检查点：每个瓦片/文本行一个原子事务、串行提交、提交序列审计、内容/模板/参数/算法版本校验。
- 实时处理、暂停后重启恢复、全量重算三条路径的自动重放比较，首个差异定位到瓦片坐标、文本行编号和字段。
- Wails 桌面入口 + 无外部前端依赖的专业深色工作台 UI。
- 本地 HTTP 实验台，便于在无 GTK/WebKit 的开发容器中执行同一套交互与验收。

## 快速开始

要求 Go 1.22+ 和可用的 C 编译器（`go-sqlite3` 使用内置 SQLite amalgamation，只需 CGO）。

```bash
go env GOPATH GOMODCACHE CGO_ENABLED
go test ./...
go run ./cmd/verify -samples samples -seed 20261124
```

启动浏览器实验台：

```bash
go run ./cmd/ocrlab -addr 127.0.0.1:8123 -samples samples -db data/workbench.db
```

打开 <http://127.0.0.1:8123>。浏览器实验台与 Wails 桌面端共享 `internal/webui/assets` 和 `internal/lab` 服务层。

重新生成样本：

```bash
go run ./cmd/samplegen -dir samples -seed 20261124
```

固定种子下样本 PNG、真值 JSON 与清单均逐字节一致。

## Windows 可重复构建

1. 安装 Go 1.22+，确认 `go version`、`go env GOPATH` 正常。
2. 安装 WebView2 Runtime（Windows 10/11 通常已内置）。
3. 安装提供 `gcc` 的 MinGW-w64 工具链（例如 MSYS2 `mingw-w64-x86_64-gcc` 或 TDM-GCC），确保 `gcc --version` 可用。
4. 开启 CGO：`$env:CGO_ENABLED = "1"`。
5. 安装 Wails CLI：

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails doctor
```

6. 运行测试、三路径验收和桌面构建：

```powershell
.\scripts\verify.ps1
.\scripts\build-desktop.ps1
```

也可以直接执行：

```powershell
go test ./...
go run ./cmd/verify -samples samples -seed 20261124
wails build -clean
```

桌面应用的数据目录为 `%AppData%\OCRWorkbench`：

- `samples/`：首次启动由本地生成器创建。
- `data/workbench.db`：SQLite 检查点。

Linux 构建 Wails 还需要 GTK3/WebKitGTK 开发包；Windows 使用 WebView2，不依赖 GTK。当前开发容器已安装 Wails CLI v2.10.2，但缺少 WebKitGTK 且无 sudo 权限，因此桌面二进制未在该容器内链接；浏览器实验台、核心、测试和 API 已完整验证。详见 `docs/VERIFICATION.md`。

## 样本清单

| 样本 | 分辨率 | 通道 | 区域 | 特征 |
|---|---:|---:|---:|---|
| `sparse_single_960x540` | 960×540 | 4 | 1 | 单行、稀疏、轻微倾斜 |
| `multiline_1280x720` | 1280×720 | 4 | 3 | 多行阅读顺序、轻微倾斜 |
| `tilted_low_1024x640` | 1024×640 | 4 | 2 | ±6.5° 倾斜、低对比度 |
| `dense_1600x900` | 1600×900 | 4 | 6 | 密集多行、长文本 |
| `sparse_gray_800x600` | 800×600 | 1 | 2 | 灰度、稀疏区域 |

每个样本都有 `manifest.json` 条目和独立 `.truth.json`。真值字段包含 `quad`、`bbox`、`text`、`char_count`、`avg_stroke_width`、`line_index`。

## 处理链

1. **加载与校验**：PNG 解码、清单/真值解析、分辨率/通道数/区域数/字符数校验、像素内容 SHA-256。
2. **预处理**：RGB(A) 亮度转灰度、全图归一化、Otsu 阈值与文字极性判断、Sobel 梯度图。
3. **瓦片检测**：固定瓦片尺寸、重叠边带由平均笔画宽联动计算；每个瓦片只在核心区拥有候选，避免重叠重复。
4. **候选提取**：8 邻域连通域、面积/尺寸过滤、水平游程中位数估计笔画宽、平均梯度特征。
5. **文本行合并**：确定性并查集，以重叠度、垂直接近、水平间隔、笔画宽比例合并；按阅读顺序稳定排序。
6. **去倾斜与切分**：在 `[-10°, +10°]` 内搜索最大行投影集中度；列投影得到字符段，过滤噪声碎片。
7. **模板匹配**：字符与模板保持长宽比归一化；精确 Dice + 容差 Dice + 角部特征 + 小位移搜索；置信度为 `[0,1]` 匹配分。
8. **状态推进**：瓦片 `pending → detecting → committed/failed`；文本行 `pending → recognizing → committed/failed`，失败可重试。
9. **检查点与统计**：每次提交写单元中间结果、内容哈希、模板/参数/算法版本和提交日志；输出质量统计与真值对照。

## 一致性与恢复语义

### 提交序列

- 瓦片按 `tile_index` 扫描顺序提交。
- 全部瓦片提交后，文本行按连续阅读顺序 `0..N-1` 提交。
- SQLite 单连接 + 进程内互斥锁 + 每单元事务；不允许交错写。
- `commits.seq` 自增，提交日志记录 `tile:*` / `line:*`、稳定 payload hash 和完整快照。

### 快照身份

重启加载时比较：

- 像素内容哈希 `image_hash`
- 模板库版本 `template_version`
- 结构参数版本 `param_version`
- 算法版本 `algorithm_version`
- 实际边带宽度 `band_width`

任一项不匹配即删除该样本的整个旧 run、tile、line、commit 快照，重新全量计算；不会部分混用。

### 参数与冻结

- `param_version` 覆盖会影响结构/几何的身份参数：瓦片尺寸、边带策略、检测最小面积、切分算法、计数容差、模板版本。
- `merge_iou`、`stroke_consistency`、`min_candidates`、`confidence_threshold` 属于运行期调参，不进入结构版本。
- 每个已提交文本行保存完整参数与合并快照；调参仅影响尚未提交的单元，不能改写已提交文本、置信度或快照。
- 若需修改结构参数，应启动 `reset` 路径；旧快照会显式显示作废字段并全量重算。

## 异常码

| 异常码 | 场景 |
|---|---|
| `E_DECODE` | 清单缺失、PNG 损坏、真值无法读取 |
| `E_META` | 分辨率/通道数/区域数/字符数不一致，包含字段名、期望值、实测值 |
| `E_PARA` | 参数越界、边带为零或达到/超过瓦片一半 |
| `E_DET` | 候选数低于配置下限、合并行数超容差 |
| `E_TMPL` | 模板版本缺失、未知版本、无可用模板 |
| `E_STATE` | 非法状态迁移；原状态和提交保持不变 |
| `E_BAND` | 边带不足导致候选触边断裂，包含瓦片与断裂坐标 |
| `E_READ` | 阅读顺序索引重复、越界或不连续 |
| `E_DIFF` | 三路径首个差异，含瓦片坐标、文本行编号、差异字段 |
| `E_STORE` | SQLite/持久化基础设施错误 |

## UI 功能

- 原图与文本行/候选/瓦片/真值四类叠加层。
- 文本行列表、状态、置信度、字符数、提交序号。
- 选中行逐字符置信度、runner-up、几何、冻结快照 JSON、构造真值对照。
- 瓦片状态矩阵与候选数，点击审计候选和 payload hash。
- 提交序列审计、异常链、实时进度和双层状态统计。
- 实时处理、恢复、全量重算、暂停、取消、失败重试。
- 参数调整（仅未提交单元）、三路径比对、结构化 JSON 导出。

## 项目结构

```text
cmd/ocrlab        浏览器实验台入口
cmd/samplegen     确定性样本生成 CLI
cmd/verify        三路径验收 CLI
internal/synth    图像/真值生成器
internal/fontlib  确定性字形与模板库
internal/imaging  解码、校验、灰度、归一化、瓦片
internal/ocr      检测、合并、去倾斜、切分、识别
internal/domain   共享模型、参数、状态机、错误
internal/store    SQLite 检查点与提交日志
internal/pipeline 双层状态机引擎、统计、三路径比较
internal/lab      Wails/HTTP 共用服务与 API 分发
internal/webui    共享前端资源（HTTP + Wails embed）
main.go           Wails 桌面入口（build tag: desktop）
```

## 验证

实际结果：

- `go test ./...`：全部通过。
- `go test -race ./internal/ocr ./internal/lab ./internal/pipeline`：通过。
- 5 个样本三路径：文本准确率 100%，行数偏差 0，瓦片/文本行/提交序列逐项一致。
- 密集样本：6 行、99 字符、45 瓦片、51 次提交，三路径一致。

完整日志与 Wails 环境说明见 `docs/VERIFICATION.md`，设计与算法细节见 `docs/DESIGN.md`。

## 约束

- 不使用 Docker 或任何容器环境。
- 生成器与验收不访问外部素材；仅本地文件。
- 真值不参与检测/识别决策。
- 当前样本文本为生成器内置大写字母、数字和少量符号；模板库版本显式固定。
