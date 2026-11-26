# IconFlow 实现说明

## 范围与包结构

- `model`：统一内部工程模型、块坐标、真值、容差、量化与覆盖参数。
- `errs`：`E_SCHEMA`、`E_LOSSY`、`E_OVF`、`E_THR`、`E_REGION`、`E_TOP`、`E_STATE`、`E_DIFF`、`E_BUDGET`、`E_CHECKPOINT` 等统一错误类型。
- `convert`：千分比/万分比、块序号/原点/绝对坐标、量化坐标、平坦/分组通道偏移的纯整型换算；所有除法余数和溢出操作数可追溯。
- `generator`：固定种子 `42042` 的三张不同分辨率图标、22 个块、解析真值与面板脏块记录。
- `schemax`：`icon-project.v1/v2/v3` JSON 装载、必填字段路径校验、默认补齐、版本导出、`E_LOSSY` 降级清单和读回语义等价比较。
- `segment`：整型灰度/Luma、量化余数定位、Sobel/Prewitt、整数平方根幅值、整型分位阈值、前景标记、4/8 连通域、碎块清理、填孔、重心/面积统计与真值容差校验。
- `state`：五阶段块状态机和面板窗口状态机。
- `checkpoint`：`Store` 接口、互斥内存存储、标准库临时文件 + `fsync` + 原子 rename 的文件存储，以及仓库内 CGO/dlopen 薄绑定实现的真实 SQLite 存储。
- `graph`：阈值刻度、量化步长、通道次序、算子版本到梯度、动态阈值、标记、统计的显式依赖图；稳定失效清单和 SHA-256 缓存键。
- `pipeline`：纯计算核心、受限 worker/队列/像素预算调度器、阶段事务提交、断点恢复、块级失败重试、三路径执行与 `E_DIFF` 比对。
- `workbench`：仅标准库 `net/http` 的本地 JSON API 与单页诊断台。
- `sampleio` / `cmd/iconflow`：样本落盘、验证报告、SQLite 检查点和诊断台命令。

## 版本口径

| 版本 | 阈值写法 | 通道排列 | 量化字段 |
| --- | --- | --- | --- |
| `icon-project.v1` | `threshold_permille`，千分比 | `pixels_flat` / `channel_sequence`，平坦交错 | 可选 `quant_step`，缺省由 v1 兼容默认值补齐 |
| `icon-project.v2` | `cut_permyriad`，万分比 | `channel_planes`，按通道分组 | `quantization.step/round/divides_extent` |
| `icon-project.v3` | `scale.threshold_per_10k`，万分比 | `color.organization=channel_grouped` + `raster.grouped` | `quantization.step/remainder_direction/extent_divisible` |

内部阈值统一为万分比整数。千分比乘 10 精确升级；万分比降千分比按声明的 `floor`、`ceil` 或 `nearest` 处理余数，并写入 `ConversionRecord`。

## 分割口径

1. 按工程声明的通道次序从原始平面取 R/G/B，使用整数权重 `299/587/114`。
2. 量化步长只做整数减法到步长网格，记录每个非零余数的块内坐标和残差。
3. Sobel/Prewitt 计算 `gx/gy`，幅值为 `isqrt(gx²+gy²)`，无浮点累积。
4. 阈值刻度本身是万分比分位；排名用整数向上取整。动态阈值为 `max(MinContrast, quantileValue)`。
5. `mag >= threshold` 形成标记；先删除小面积连通域，再从边界泛洪识别并填充孔洞，最后输出连通域面积和毫像素重心。

## 检查点与恢复

每个阶段提交记录包含：

- 块坐标、阶段、状态和摘要；
- 动态阈值；
- 阶段缓存键；
- 块输入哈希与阶段内容哈希；
- 参数版本和算子版本；
- 可恢复完整中间产物 JSON。

SQLite 实现用 `BEGIN IMMEDIATE; INSERT ...; COMMIT;` 包装单条阶段/窗口事务，并在 Store 内互斥串行化。文件实现用同目录临时文件、`fsync`、原子 rename 和目录 fsync 达到等价的全有/全无提交语义。

## 缓存与失效

阶段键由以下材料经 SHA-256 得到：

- 块原始像素内容哈希；
- 阈值版本（阈值万分比值 + 参数版本）；
- 量化版本（步长、取舍方向、整除声明、清理规则）；
- 通道次序与排列哈希；
- 算子版本；
- 上游阶段缓存键。

块级编辑只改变目标块的 effective params，因此未受影响块能从检查点恢复；全局编辑会改变所有相关块。影响清单按块、节点和依赖路径排序，字段枚举顺序不影响输出。

## 标准库约束与 SQLite 的说明

Go 标准库不包含 SQLite 驱动，同时任务又要求“基于 SQLite”和“不引入第三方 Go 包”。当前处理是：

- 不导入任何第三方 Go module；
- `checkpoint/sqlite_cgo.go` 用仓库内 CGO 声明和 `dlopen("libsqlite3.so.0")` 调用本机已存在的 SQLite；
- `CGO_ENABLED=0` 或非 Linux 构建使用 `checkpoint/sqlite_stub.go`，此时标准库 `FileStore` 仍提供原子检查点和完整流水线恢复能力。

本环境已确认生成的 `artifacts/iconflow.sqlite` 可被外部 Python `sqlite3` 打开并查询。
