# OCR Workbench

本地、确定性、可断点续跑的 OCR 文字检测与识别工作台，用于游戏截图和互动媒体素材。项目只使用本机回环地址与本地合成样本，不依赖 Docker、外部图片、网络服务或运行时下载。

## 交付内容

- `cmd/ocrgen`：固定种子合成文字图像与真值清单（多分辨率、单行/多行、倾斜、低对比度、稀疏/密集）。
- `cmd/ocrbench`：一次性执行实时、断点恢复、全量重算三条路径，输出 JSON 并验证一致性。
- `cmd/ocrworkbench`：本地 Web 工作台，展示图片、检测框、文本行、逐字符置信度、瓦片矩阵、统计与三路径结果。
- `cmd/ocrverify`：独立验证器，可仅凭 SQLite 持久化单元或导出 JSON 与真值指纹重算指标并定位首个违规行。
- `internal/*`：加载校验、图像处理、模板匹配、双状态机、SQLite 检查点、并发池和验证逻辑。
- `vendor/`：已固化的纯 Go SQLite 依赖，可在离线环境构建。

## 快速开始

要求 Go 1.22+。本仓库已将依赖执行 `go mod vendor`，并建议使用纯 Go 网络实现 tags：

```bash
go env -w GOFLAGS=-tags=netgo
go env GOCACHE GOMODCACHE
```

生成固定种子样本并运行测试：

```bash
go run ./cmd/ocrgen -out data -seed 42
go test ./...
```

三路径一次性验证：

```bash
make bench
make verify
```

启动本地界面（只接受回环地址）：

```bash
make serve
# http://127.0.0.1:8099
```

仅允许 `127.0.0.1`、`::1` 或 `localhost`。传入 `0.0.0.0` 等地址会直接被拒绝。

## 样本与真值

`ocrgen` 使用内置 5×7 位图字体和固定种子，生成：

| 图像 | 分辨率 | 排版 |
| --- | ---: | --- |
| `dense_1280` | 1280×720 | 8 行，双列密集，正负倾斜，低对比度 |
| `sparse_960` | 960×540 | 3 行稀疏布局 |
| `single_800` | 800×450 | 单行低对比度 |
| `mixed_1024` | 1024×600 | 6 行，稀疏与密集混合 |

真值只来自绘制构造记录：四点包围框来自实际墨迹局部坐标经同一仿射旋转得到，文本、字符数和平均笔画宽均直接记录。`manifest.json` 包含每张图的尺寸、通道数、区域数、SHA-256 和统一真值指纹。

## 处理链

1. **加载与结构校验**
   - 解码失败返回 `E_DECODE`。
   - 尺寸、通道、声明区域数、内容哈希、文本字符数不一致返回 `E_META`，并指出字段名。
   - 四点坐标必须有限、面积非零、无退化边。

2. **预处理与检测**
   - 灰度化 + Otsu 类别对比度归一化 + 局部梯度/方差自适应补充，兼顾低对比度文本和背景条纹。
   - 8 邻域连通域、最短横/纵游程中位数估计笔画宽度。
   - 按水平间距、中心对齐、笔画宽一致性和垂直邻接合并为文本行。
   - 按连通域中心回归估计倾斜角，沿该方向投影得到紧致四边形包围框。

3. **瓦片边界**
   - 每个瓦片由不重叠 core 与两侧 overlap band 组成。
   - `RequiredBand = ceil(merge_iou * min(tile_width,tile_height) * 0.5)`；不足时返回 `E_BAND`，携带瓦片编号与边界位置。
   - 跨瓦片候选按 IoU、包含率和中心距离聚类；保留主方向，纵向范围外接、横向范围加权平均，避免短碎片污染倾斜行面积。

4. **字符切分与识别**
   - 在文本行局部坐标系反旋转，按连通域与局部 X 重叠聚合旋转栅格产生的碎片。
   - 归一化到共享 5×7 模板网格，面积占比采样后与本地位图模板库做 Dice + 像素一致度匹配。
   - 输出逐字符/逐行置信度：`0.5*best + 0.5*clamp(margin*scale)`，恒在 `[0,1]` 内且随匹配裕度单调增加。
   - 空行保留为 `is_empty=true`，`text=""`，置信度为 0，不静默丢弃。
   - 切分参数越界返回 `E_PARA`，附带字段名和合法区间。

## 状态机与冻结语义

瓦片状态：

```text
pending -> detecting -> committed
                     -> failed -> pending
```

文本行状态：

```text
pending_recognition -> recognizing -> committed
                                   -> failed -> pending
```

非法迁移返回 `E_STATE` 且原状态保持不变。进度分别由已提交瓦片数和已提交文本行数表示。已提交行冻结切分参数、包围框、文本与置信度；`fsm.LineRuntime` 和检查点恢复路径均遵守该约束。

## SQLite 检查点

每个已提交瓦片或文本行在独立事务中串行写入 SQLite，记录：

- 单元中间结果 JSON
- 内容哈希
- 模板库版本
- 参数版本
- 算法版本
- 稳定提交序号

恢复时校验整个快照及每个单位。任一内容哈希或版本不匹配会将旧快照整体标记为 `valid=0`，创建新快照并全量重算，不会混用旧单元。并发提交由单连接 + 写锁串行化，加载按稳定序号排序。

当前版本：

- 模板：`bitmap5x7-v1`
- 算法：`ocr-pipeline-v3`
- 参数哈希：由检测参数与切分参数规范化 JSON 计算

## 三路径一致性

- **real-time**：正常逐瓦片/逐行处理并写检查点。
- **resume**：先在第 3 个瓦片提交后注入崩溃，再恢复至第 2 个文本行提交后注入崩溃，最后继续完成。
- **full**：不读旧检查点，在全内存中一次性重算。

比较字段包括阅读顺序索引、逐行文本、逐字符置信度、包围框、内容哈希、瓦片/文本行提交序列、进度与统计。

## 并发与异常边界

- 检测和识别通过固定大小 worker pool 并行；提交始终按瓦片扫描顺序与文本行阅读顺序稳定执行。
- 每 worker 有像素缓冲上限，共享缓冲池耗尽时阻塞排队，不会丢单元。
- 超限单元返回显式 `ErrBufferLimit`；单单元 panic/error 仅将该单元置为失败。
- 已提交结果不因相邻单元失败而改变；失败单元未写检查点，重新运行即可只补算未提交单元。
- 支持暂停、继续、取消；取消不会提交后续部分结果。

## 独立验证器

验证导出 JSON：

```bash
go run ./cmd/ocrverify \
  -manifest data/manifest.json \
  -realtime artifacts/exports/realtime.json \
  -resume artifacts/exports/resume.json \
  -full artifacts/exports/full.json
```

验证 SQLite 持久化单元：

```bash
go run ./cmd/ocrverify -manifest data/manifest.json -db artifacts/bench.db
```

验证器不信任已保存统计，会重新计算：

- 文本行数及阅读顺序
- 文本行包围框与真值多边形 IoU
- 逐字符置信度范围、逐行平均值
- 全图置信度分布
- 空行显式标记
- 内容哈希与稳定提交序列

失败时输出首个违规 `image_id` 和文本行编号。

## 实际验证结果

本机 `go test ./...` 全部通过；`make bench && make verify` 结果：

```text
images=4 lines=18 chars=126 empty=0
min_iou=0.7154
line confidence range=[0.5714, 0.7106]
OK: realtime, resume and full are identical
OK: verified 3 persisted run(s)
OK: verified 4 persisted run(s)
```

逐图检测与真值：

| 图像 | 检测行/真值行 | 最小 IoU | 平均 IoU | 平均置信度 |
| --- | ---: | ---: | ---: | ---: |
| dense_1280 | 8/8 | 0.8121 | 0.9221 | 0.6805 |
| sparse_960 | 3/3 | 0.9567 | 0.9617 | 0.6432 |
| single_800 | 1/1 | 0.7154 | 0.7154 | 0.6319 |
| mixed_1024 | 6/6 | 0.8784 | 0.9397 | 0.6355 |

完整机器可读结果写入 `artifacts/verification.json`，三路径导出位于 `artifacts/exports/`。

测试覆盖：

- 固定种子样本字节级确定性，以及不同种子的布局真值差异。
- `E_DECODE`、`E_META` 字段定位、非有限/退化框。
- 检测行数、逐行文本、IoU、倾斜面积偏差、候选可重复性、整图与分块一致性。
- `E_PARA` 字段/区间、`E_BAND` 瓦片/边界、置信度单调性与空行。
- 非法状态迁移、进度计数、提交后参数/结果冻结。
- 三路径完全相同、两次断点恢复、失败/panic 行隔离与成功后补算。
- SQLite 版本/哈希不匹配整快照作废、并发写串行化、稳定装载顺序。
- worker 并行度、缓冲上限、错误隔离、暂停/取消。
- 本地 HTTP 状态/图片接口、非回环拒绝、独立验证器首个违规定位。

## 目录

```text
cmd/ocrgen/          确定性样本生成器
cmd/ocrbench/        三路径无界面执行器
cmd/ocrworkbench/    本地 Web 服务
cmd/ocrverify/       独立验证器
internal/core/       类型、几何、错误码、参数校验
internal/font/       5×7 本地模板库
internal/sample/     样本构造与真值
internal/loader/     解码与元数据校验
internal/vision/     预处理、检测、合并、切分、识别
internal/fsm/        瓦片/文本行状态机与冻结语义
internal/store/      SQLite 版本化检查点
internal/pipeline/   并发流水线与三路径
internal/verify/     独立指标重算与差异定位
internal/app/        应用编排
internal/web/        回环 HTTP API 与界面
```
