# MaskPipe 技术说明

MaskPipe 是一套面向游戏美术素材的本地掩码分割、受限预算并行预览、SQLite 检查点恢复与独立验证工具。模块名为 `maskpipe`，Go 版本固定为 `1.22`，SQLite 驱动使用纯 Go 的 `modernc.org/sqlite`。

## 目录

- `cmd/maskpipe`：`generate`、`validate`、`run`、`threeway`、`verify`、`serve` 命令。
- `internal/model`：清单、参数、区域几何、状态和统一错误码。
- `internal/sample`：固定种子样本生成、PNG/真值清单加载与校验。
- `internal/vision`：颜色/梯度区域生长、阈值、形态学、连通域几何和最优匹配。
- `internal/pipeline`：分块规划、四态状态机、预算调度、预览、三路径比对和检查点编排。
- `internal/store`：版本化 SQLite schema、原子阶段事务和串行写入。
- `internal/verify`：不导入主生长路径的独立哈希、块几何和真值验证器。
- `internal/web`：嵌入式无外网诊断界面与 JSON/PNG API。
- `configs/params.example.json`：Windows 示例参数。
- `scripts`：Windows `.bat` / PowerShell 与本地 Bash 验证脚本。

## 确定性样本

生成器默认使用种子 `424242`，产出三个场景：

| 场景 | 分辨率 | 分块 | 重点 |
| --- | ---: | ---: | --- |
| `blocks` | 192×128 | 64×64 | 高对比色块、矩形、椭圆 |
| `low-contrast` | 240×160 | 80×80 | 渐变背景、低对比目标 |
| `embedded-noise` | 256×192 | 64×64 | 嵌入式噪声、四个目标 |

每个场景输出原图、像素级真值掩码和 `manifest.json`。清单声明区域重心、面积、外接框、区域数、真值指纹、分块网格、worker 数、每线程像素缓冲上限、队列预算、预览延迟和质量/分歧容差。

## 视觉链

1. 用 3×3 邻域平均估计种子颜色。
2. 以确定性 FIFO 四邻接遍历执行区域生长；邻居顺序固定为上、右、下、左。
3. 接受条件包含 RGB 欧氏颜色距离和 Sobel 归一化梯度；强颜色匹配的边界像素不会因外侧梯度峰值被误删。
4. 用颜色/梯度评分做二值阈值化。
5. 使用方形结构元素执行开运算和闭运算；越界像素按边缘复制处理，避免分块边缘侵蚀缝。
6. 用 8 邻接连通域计算面积、重心、外接框和基于网格边的闭合轮廓。
7. 用匈牙利算法做预测区域与真值区域的最小代价匹配。

同一图像、参数和种子下没有随机 map 迭代或线程标识分支，因此 worker 数量和任务完成顺序不改变结果。

## 错误码

所有业务错误均为 `model.Error`，包含 `code`、`field`、`message` 和可选 `details`。

| Code | 触发点 |
| --- | --- |
| `E_DECODE` | PNG 损坏或无法解码 |
| `E_META` | 清单字段缺失、尺寸/通道/分块网格/指纹不一致，`field` 指出字段 |
| `E_PARA` | `color_tolerance`、`gradient_threshold`、`structure_size` 等参数越界 |
| `E_REGION` | 区域数量不一致，列出多发/缺失区域编号和面积 |
| `E_QUAL` | 重心、面积或外接框偏差超过声明容差 |
| `E_STATE` | 非法阶段状态迁移，原状态保持不变 |
| `E_SEED` | 种子越界、落在背景或真值区域外 |
| `E_DIVERGE` | 预览与全精度结果的区域数、重心、面积或像素差异超差 |

## 分块状态机

每个 `(chunk_x, chunk_y, stage)` 都是一个状态实体，阶段为 `grow`、`clean`、`stats`，状态为：

```text
pending -> processing -> committed
                      \-> failed -> pending（仅重试）
```

`committed` 是冻结态，不能再次迁移。提交时记录参数版本、算法版本、降采样倍率、摘要、内容哈希和几何 JSON。总进度为已提交阶段数 / `chunk_count * 3`。

## 预算调度

- `worker_count`：worker 信号量上限，测试会观测真实峰值并行度。
- `pending_task_queue_budget`：buffered task channel 容量。
- `pixel_buffer_per_thread`：单块按约 5 份像素缓冲核算；若 `pixels * 5` 超过上限，构造服务时返回 `E_PARA field=declared.budget.pixel_buffer_per_thread`。
- 当 task channel 不能立即入队时，调度器记录 `queue_budget_exhausted`、块、阶段、预算和阻塞等待时长，然后阻塞入队；不会丢弃任务。

SQLite 使用 `SetMaxOpenConns(1)`，且 `Store.CommitStage` 有互斥锁和单事务，保证写入不交错。每行保存 `canonical_order = chunk_index * 3 + stage_index`，读取时按该稳定顺序输出阶段序列。

## 参数编辑与预览

参数版本由包含种子点的规范化参数 JSON 计算 SHA-1 前缀。拖动滑块时：

- 选择不超过 12288 采样像素的最大固定倍率；内置样本均选择 `0.5`。
- FIFO 访问上限从 `20000~40000` 降到 `1024`。
- 记录实际倍率、降采样尺寸、原始像素数、目标像素数、迭代数和选择规则。
- 预览掩码最近邻放大回原图尺寸，计算区域数、最大重心偏差、最大面积相对误差、像素不一致率、首个不一致像素和局部差异最密集像素。

停止拖动后，Web 端用同一组种子和派生参数版本执行全精度重算。已提交阶段的参数版本不会被活动会话中的后续编辑改写；新参数只进入尚未开始的任务。

## SQLite 检查点与三路径

`snapshot_meta` 保存算法版本、参数版本、尺寸、分块、种子、真值指纹和完整参数 JSON。`stages` 保存：

- 块坐标、阶段、规范顺序；
- 参数/算法版本、降采样倍率；
- `summary_json`、`geometry_json`；
- raw-gray8 产物或 stats JSON 的 `blob`；
- `content_hash = sha256(blob)`。

恢复时先校验元数据版本和每行哈希。任何算法/参数版本或哈希不一致，都判定快照不可用，删除该 snapshot 的元数据和阶段后全量重算，不混用旧阶段。

`pipeline.RunThreeWay` 会：

1. 只提交 `resume.db` 的第 0 块，模拟断点；
2. 用新 Store 打开同一 DB 执行 `resume`；
3. 在 `live.db` 执行实时增量；
4. 在 `full.db` 执行全量重算；
5. 比较最终掩码字节、几何哈希和规范阶段哈希序列，定位首个不一致像素与阶段 ID。

## 独立验证器

`internal/verify` 不导入 `internal/pipeline` 或 `internal/vision`，只使用 `database/sql`、标准库和 SQLite 驱动。它：

- 逐行重算 SHA-256；
- 校验参数/算法版本；
- 从 `clean` blob 拼接整图；
- 用独立实现的连通域统计重算每块和全图几何；
- 对照 SQLite 中的 `geometry_json` 字段；
- 对照 manifest 中的真值指纹和真值区域；
- 输出首个违规块坐标、阶段、字段、期望值和实际值。

命令：

```bat
set CGO_ENABLED=0
go run .\cmd\maskpipe verify -db reports\threeway-blocks\dbs\full.db -manifest samples\local-seed\manifest.json -scene blocks -out reports\threeway-blocks\verify-cli.json
```

## Web 诊断界面

```bat
scripts\serve-demo.bat
```

打开 `http://127.0.0.1:8088`。界面展示原图、真值、阶段产物、最终叠加、轮廓偏差热力图、块阶段矩阵、预算峰值、排队明细、独立验证、三路径结果和参数预览；支持逐块查看、失败阶段重试、暂停/继续/取消以及掩码/报告导出。所有静态资源通过 `go:embed` 打包，不使用 CDN。
