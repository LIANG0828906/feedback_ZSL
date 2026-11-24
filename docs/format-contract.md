# 格式与持久化契约

## 版本差异

| 属性 | V1 | V2 | V3 |
|---|---|---|---|
| 判别字段 | `format_version: foreground-v1` | `schema: foreground/v2` | `spec: virtualstudio/foreground@3` |
| 尺寸 | `resolution.w/h/channels` | `canvas.width/height/channels` | `dimensions.width/height/channels` |
| 层顺序 | `layers[].z_index` | 有序 `stack[]` | `order_links[]` 的 `above` 引用链 |
| 可见性 | 布尔 `visible` | `visibility: VISIBLE/HIDDEN` | 整数 `enabled: 0/1` |
| Alpha | 8 位 straight | 8 位 straight，`pixel_encoding.alpha` | 16 位 premultiplied，`pma: true`；兼容 8 位旧文档 |
| 时间 | 微秒/帧 | 毫秒/帧 | tick/帧 + tick 频率 |
| 样本 | 基准/道具/角色三层或更多 | 基准/道具/角色三层或更多 | 基准/道具/角色三层或更多 |

内部模型固定：

- 层顺序为数组下标从底到顶。
- 层像素规范化为 8 位 sRGB straight RGBA。
- 帧时间统一为纳秒。
- 输出尺寸、通道数、时间戳均由内部模型计算，不从合成指令或线程状态推断。

V3 使用 16 位编码保存预乘样本，使任意 8 位 straight 像素经过 V1→V3→内部模型往返不丢精度；内部仍为 8 位，避免输出通道或缓存格式分叉。

## 错误契约

| 错误码 | 场景 | 定位 |
|---|---|---|
| `E_VER` | 未知版本、必填字段缺失、引用非法 | JSON 字段路径 |
| `E_CYCLE` | V3 `above` 引用成环 | `order_links` + 环上首个层标识 |
| `E_PARA` | 不透明度、尺寸、采样点、窗口或缓冲参数越界 | 字段路径 |
| `E_STATE` | 非法窗口状态迁移 | `windows[i].state` |
| `E_DECODE` | base64、像素长度、样本损坏 | 像素字段路径 |
| `E_LOSSY` | 目标版本不能表达的属性 | 降级报告路径、原值、处置 |
| `E_COMPARE` | 自动对比发现首个不一致 | 窗口、帧、像素坐标 |

## SQLite 检查点

表 `projects` 保存算法版本、源格式、基础内容哈希、窗口大小、帧数、工程版本、目标格式和当前场景/指令 JSON。

表 `checkpoints` 每个窗口一行，包含：

- 窗口编号、帧范围和稳定写入序号。
- 参数版本、内容哈希、帧摘要。
- 源格式版本、算法版本。
- 冻结的层参数/可见性/时间基准快照。
- 合成帧 JSON（含逐帧采样统计）。

写入使用单连接、进程内互斥和事务，提交顺序严格按窗口编号。读取时逐窗口重算参数版本、内容哈希、帧摘要与帧哈希；任一不匹配即删除该工程全部快照。

## 增量失效

依赖图节点包括层、帧窗口和 `export:v1/v2/v3`。编辑层后逐窗口比较渲染贡献哈希：

- 只有层在该窗口至少一帧实际可见且不透明度大于 0 时，参数变化才使该窗口失效。
- 时间基准变化使所有含可见内容的窗口失效。
- 即使层完全隐藏，描述参数变化仍使导出阶段失效。
- 失效窗口先从事务性缓存删除；其余窗口继续复用。

