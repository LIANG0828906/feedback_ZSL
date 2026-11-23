# 设计与算法说明

## 1. 目标与边界

工作台解决两条主线：

1. 从确定性游戏截图样本中检测文本行、切分字符、模板匹配识别，并给出可解释置信度。
2. 在暂停、退出、重启、调准入参数和全量重算之间，保持已提交结果与提交序列严格一致。

真值由生成器在绘制时直接记录：文字、四角坐标、字符数、构造笔画宽。检测/识别代码没有读取 `TruthFile` 的入口。真值只在以下位置使用：

- 加载阶段校验清单声明和文件结构。
- 引擎合并后校验行数容差。
- UI/导出统计 truth IoU 与文本匹配率。
- 验收比对。

## 2. 确定性样本生成

`internal/synth` 使用固定样本规格和 `rand.NewSource(seed + 104729*(i+1))`：

- 背景：渐变 + 固定伪随机噪声 + 扫描线。
- 前景：内置 `bitmap5x7-v1` 字形，按整数 `scale` 绘制，无外部字体/图片。
- 倾斜：最近邻旋转透明图层，真值四点框同步旋转。
- 灰度样本：最终转为单通道 `image.Gray` 后 PNG 编码。
- 真值：只写绘制时的记录，不在加载后反推。

相同 Go 版本与相同 seed 下，PNG、truth JSON、manifest JSON 逐字节一致。测试 `TestGenerationIsByteDeterministic` 验证。

## 3. 图像校验

`imaging.LoadSample` 顺序：

1. 读取 `manifest.json`；缺失/JSON 错误 → `E_DECODE`。
2. 读取 PNG；损坏 → `E_DECODE`。
3. 校验宽、高、通道数、manifest 像素哈希；不一致 → `E_META`，填写 `field/expected/actual`。
4. 读取 truth JSON；缺失/格式错误 → `E_DECODE`。
5. 校验 truth 的 schema、image id、宽高、通道、`region_count == len(regions)`、manifest 区域数、每行字符数。

通道数按解码后的具体图像类型：Gray/Gray16 = 1，RGBA/NRGBA/RGBA64/NRGBA64 = 4，Paletted = 3。

## 4. 预处理

- 亮度：`299R + 587G + 114B`。
- 归一化：全图最小/最大拉伸到 `0..255`。
- Otsu 全局阈值。
- 极性：低于/高于阈值的像素数较少侧视为文字，兼容亮字暗底和暗字亮底。
- 梯度：Sobel 幅值 `/4`，作为候选的 `gradient_score` 审计特征。

全图共享阈值，避免瓦片边界因局部统计不同产生候选漂移。

## 5. 瓦片与边带

参数：

- `tile_size`：瓦片 ROI 尺寸。
- `band_factor`：与全图估计平均笔画宽联动，`band = round(avg_stroke * band_factor)`，且不小于 `min_band`。
- `band <= 0` 或 `band >= tile_size/2` → `E_PARA`。

布局：

- `stride = tile_size - 2*band`。
- 瓦片 ROI 从 `(col*stride, row*stride)` 开始，尺寸最多 `tile_size`。
- 核心区在非图像边缘收缩 `band`；核心区无缝且有唯一归属。
- 连通域按包围框中心归属核心区，减少重叠瓦片重复候选。

若核心区中心的连通域触到瓦片边界、且该边界不是图像边界，说明边带不足以看到完整字形，返回 `E_BAND` 和瓦片/断裂坐标。

## 6. 候选与行合并

候选 = 8 邻域连通域。过滤条件：

- 最小面积 `detector_min_area`。
- 宽高至少 2。
- 非接近瓦片大小的巨型连通域。
- 中心落在当前瓦片核心。

笔画宽估计：对连通域内像素统计水平游程长度，排序后取 35% 分位，至少 1。

文本行合并使用并查集，对所有候选确定性两两判断：

- 笔画宽比例 `min/max >= stroke_consistency`。
- 垂直重叠率 `>= merge_iou`，或垂直间隙 `<= 0.42*max_height`。
- 水平间隙 `<= 1.35*max_height`。

合并键来自几何/像素哈希，不依赖瓦片处理顺序或线程 ID。候选先按 `(y,x,h,w)` 稳定排序，并查集 root 取较小索引，分组与阅读顺序均确定。

阅读顺序：先按行中心 y，再按 x 稳定排序；赋连续索引后再次校验无重复、无越界、无空洞；冲突 → `E_READ`。

整图单块检测与分块检测调用同一候选过滤和合并函数。测试 `TestTiledAndWholeDetectionAgree` 在 5 个样本上验证逐候选包围盒、逐行包围盒和笔画宽一致。

## 7. 识别

1. 按检测行包围盒扩张固定 padding 裁剪。
2. 局部 Otsu 二值化，移除小于 `0.10*stroke²` 的噪声连通域。
3. 在 `deskew_max_deg` 范围内按 `deskew_step_deg` 搜索最佳纠正角；评分是旋转后水平投影平方和。
4. 裁到 ink bbox，按列投影得到字符段；小间隙片段按 `min_gap_ratio` 合并。
5. 字符保留长宽比归一化到 `template_width × template_height`。
6. 模板匹配：
   - 精确 Dice：`2*intersection/(sumA+sumB)`。
   - 1px 容差 Dice。
   - 四角 ink 比例相似度。
   - `(-2..2, -2..2)` 小位移搜索。
   - 最终分 `0.60*exact + 0.25*tolerant + 0.15*corner`。
7. 最佳模板字符与 runner-up 写入结果。逐字符置信度即归一化匹配分；逐行置信度为字符置信度均值。
8. 无字符段时 `text=""`、`empty=true`、`confidence=0`，不丢弃该行。

模板版本缺失/未知/无可用模板 → `E_TMPL`。切分参数、模板版本、切分版本和置信度阈值都写入行快照。

## 8. 状态机

瓦片：

```text
pending -> detecting -> committed
                    \-> failed -> detecting
pending ----------------------> failed (engine guard)
```

文本行：

```text
pending -> recognizing -> committed
                      \-> failed -> recognizing
pending ----------------------> failed (engine guard)
```

`committed` 为终态；非法迁移返回 `E_STATE`，更新语句不执行。store 在事务内再次读取当前状态并校验，防止服务层绕开状态机。

进度分别由 `committed_tiles/tile_count` 和 `committed_lines/detected_lines` 表示。

## 9. SQLite 检查点

表：

- `runs`：样本、身份哈希、结构参数版本、实际边带、平均笔画宽、初始/当前参数、状态与错误。
- `tiles`：几何、状态、候选 JSON、payload hash、commit seq。
- `lines`：几何、状态、识别结果、完整冻结快照 JSON、payload hash、commit seq。
- `commits`：全局自增 seq、kind、unit key、稳定 payload hash、payload JSON。

每个提交事务：

1. 读取并校验状态。
2. 校验顺序：瓦片必须按索引顺序；行提交前所有瓦片必须提交；行必须按阅读顺序。
3. 插入 `commits` 行取得自增 seq；payload 显式包含 `image_hash`、`template_version`、`param_version`、`tuning_version`、`algorithm_version` 及单元中间结果。
4. 更新单元状态和中间结果。
5. 提交事务。

SQLite 连接 `SetMaxOpenConns(1)`、WAL、`synchronous=FULL`、`_txlock=immediate`，加进程内互斥锁。提交不会交错。

稳定 payload hash 排除墙钟时间；时间只作为审计字段保存，避免三条路径因为执行时刻不同产生 hash 差异。

## 10. 恢复与作废

`OpenOrCreateRun` 查同一 `sample_id` 的最新 run，比较身份字段。任一项不一致，在同一事务中删除旧 commits/lines/tiles/run，创建新 run。返回值包含 `invalidated` 字段列表，UI 显示作废原因。

恢复时，持久化的 `detecting`/`recognizing` 单元被视为上次进程中断，转为 `failed`，后续引擎会按失败重试路径重新执行；`committed` 永不重算。

## 11. 三路径比较

`pipeline.Compare` 在独立临时 SQLite 库运行：

- **live**：一次连续执行。
- **resume**：提交 3 个单元后暂停，关闭数据库；重新打开同一库，从 committed 状态继续。
- **reset**：新库全量执行。

比较项：

- 每个瓦片几何、状态、候选顺序/内容、payload hash、commit seq。
- 每个文本行阅读索引、文本、空行标记、接受标记、置信度、逐字符结果、payload hash、commit seq。
- 完整 commit 序列 `(seq, kind, unit_key, payload_hash)`。
- 关键统计（行/字符/置信度/准确率）。

首个差异返回 `E_DIFF`，并标记 `tile_index/tile_x/tile_y`、`line_index`、`difference_field`。

## 12. UI 架构

- `internal/lab.Service` 统一处理运行会话、异步状态、暂停/恢复/取消、调参、导出、比较。
- HTTP `/api/call` 与 Wails `App.Call(method,payload)` 共用 JSON 信封。
- `internal/webui/assets` 同时嵌入 HTTP 实验台和 Wails `assetserver`。
- 前端不依赖 Node/npm 或外部 CDN；Canvas 绘制原图和叠加层，轮询与 Wails 事件都可用。
