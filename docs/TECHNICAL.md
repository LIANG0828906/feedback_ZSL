# 视频帧取证引擎 — 技术说明

## 1. 环境与本地依赖（无 Docker、无系统安装）

全部依赖以本地包形式位于用户目录，工程不依赖网络与系统级安装：

| 组件 | 位置 | 说明 |
| --- | --- | --- |
| GCC 15 / CMake 3.30 | `/home/liang/.local` | C++17，`std::filesystem` 可用 |
| Qt 6.10.2 | `~/.local/qt6/usr` | 本地 `.deb` 解压，Core/Gui/Widgets/Concurrent/Test |
| OpenCV 4.10.0 | `~/.local/opencv4/usr` | 仅 core/imgproc，本地 `.deb` 解压 |
| 传递依赖 | `~/.local/deps/usr/lib` | 运行库符号链接（TBB/OpenBLAS/xcb/harfbuzz 等） |

OpenCV 上游 `OpenCVModules.cmake` 引用了未安装的模块，工程在
`cmake/OpenCVLocalConfig.cmake` 中只声明实际使用的 `opencv_core`、
`opencv_imgproc` 两个导入目标，不修改第三方原始文件。

构建前 `source scripts/env.sh` 注入 `LD_LIBRARY_PATH` 与 Qt 插件路径。

## 2. 自定义二进制格式 `.vfseq`

小端序，结构如下：

```
magic[4] = "VFSE"
u32 formatVersion | u32 width height channels frameCount
u64 baseTimestampUs | u64 framePeriodUs
重复 frameCount 次的帧头表:
    u32 index | u64 timestampUs | u32 dataLength | u8 sha256[32]
u32 payloadMagic = 0x4C594150 ("PAYL")
重复 frameCount 次的原始像素负载（长度=dataLength）
```

结构完整性规则（对应业务要求 1）：

- magic/版本错误、帧头或负载截断、声明长度与文件实际字节矛盾、校验字段
  全零（缺失）一律返回 `E_FRAME_INVALID`，解析立即失败，不输出任何
  “部分可信”的帧。
- 每帧 SHA-256 在负载上实时计算并与帧头校验字段比对。
- 首个失配字节同时给出帧内偏移与文件绝对偏移；字节级定位依赖可选的
  `.vref` 原始像素参考容器（与 `.vfseq` 同构，由样本生成器同步产出）。

## 3. 模块与代码映射

| 业务要求 | 实现位置 |
| --- | --- |
| 1 格式解析/哈希/失配偏移 | `src/frame_sequence.cpp`, `src/frame_sequence_writer.cpp` |
| 2 依赖图/环与悬空/级联路径 | `src/dependency_graph.cpp` |
| 3 清单版本与锚点语义差异 | `src/manifest.cpp` |
| 4 增量校验/缓存复用 | `src/cache_store.cpp`, `src/forensic_engine.cpp` |
| 5 快照/哈希链/原子发布/恢复 | `src/snapshot.cpp` |
| 6 后台线程/E_BUSY/取消隔离 | `src/forensic_worker.cpp` |
| 7 时间轴 UI/JSON 报告 | `src/timeline_widget.cpp`, `src/main_window.cpp`, `src/report.cpp` |
| 8 确定性样本/轨迹日志/演示 | `tools/vf_generate.cpp`, `scripts/demo.sh` |

### 关键设计

- **级联方向**：清单边为 `child -> parents`；帧哈希失败后在反向邻接表
  （`parent -> children`）上 BFS，所有可达下游帧标记为不可信，并为每帧
  保留一条具体传播路径（如 `3>4>5`）与受影响边集合。
- **环/悬空先于结论**：Kahn 拓扑校验失败返回 `E_DEP_CYCLE`，引用越界
  返回 `E_DEP_DANGLING`，均在任何可信结论产生之前中止。
- **版本语义**：v2 清单必须显式重述全部锚点（分段起止、水印坐标、参考
  类型）；缺失即 `E_MANIFEST_VERSION`，禁止从 v1 隐式推断。版本变化时
  输出 `ADDED/REMOVED/RANGE_CHANGED/WATERMARK_CHANGED/REFERENCE_TYPE_CHANGED`
  差异摘要。
- **缓存键**：帧缓存键同时包含声明校验和与实际内容哈希，局部重编码改变
  像素即改变键，杜绝“哈希失配却静默复用旧可信结论”。
- **快照与发布**：`QSaveFile` 临时文件 + 原子重命名；哈希链
  `recordHashN = SHA256(prevRecordHash || 规范化快照体)`。恢复先验链，
  断链/缺字段返回 `E_RECOVERY_INVALID`，不覆盖旧快照。
- **后台隔离**：同一序列同时只允许一个任务，重复提交 `E_BUSY`；UI 只经
  `readPublished()` 读取最新已发布快照，永不展示中间级联状态；取消在帧
  循环边界生效，任务对象析构即释放文件句柄，已发布结果保持可读。
- **确定性**：样本像素来自固定种子 LCG + 纯函数索引，无时钟/熵输入；
  发布时间戳由 `--clock` 固定，相同种子与操作序列下轨迹日志逐字节一致。

## 4. 错误码

| 码 | 名称 | 触发场景 |
| --- | --- | --- |
| 1 | `E_FRAME_INVALID` | 结构矛盾、截断、校验字段缺失 |
| 2 | `E_DEP_CYCLE` | 依赖图存在环 |
| 3 | `E_DEP_DANGLING` | 引用不存在的帧/锚点越界 |
| 4 | `E_MANIFEST_VERSION` | 版本不支持、锚点语义缺失、隐式迁移 |
| 5 | `E_RECOVERY_INVALID` | 快照哈希链断裂或字段缺失 |
| 6 | `E_BUSY` | 同序列重复提交/取消 |
| 7 | `E_REPORT_INVALID` | 报告必需证据字段缺失 |
| 8 | `E_IO` | 文件读写/原子提交失败 |
| 9 | `E_INTERNAL` | 兜底内部错误 |

## 5. 构建、测试与演示

```bash
source scripts/env.sh
cmake -S . -B build -DCMAKE_PREFIX_PATH="$QT6_PREFIX" -DCMAKE_BUILD_TYPE=Release
cmake --build build -j8
(cd build && ctest --output-on-failure)     # 5 个 Qt Test 测试套件
./scripts/demo.sh                           # 8 阶段端到端脚本演示
```

GUI（无头环境使用 offscreen 平台）：

```bash
QT_QPA_PLATFORM=offscreen ./build/vf_gui \
  --workdir artifacts/demo/workdirs/medium_reencode \
  --sequence artifacts/demo/samples/medium_reencode.vfseq
```

## 6. 实际验证结果（2026-11-21，Asia/Shanghai）

- 单元/集成测试：`5/5 passed`（帧格式、依赖级联、清单版本、增量快照、
  后台 worker）。
- 脚本演示 `scripts/demo.sh`：8 个阶段全部退出码 0。
- 局部重编码样本：帧 3、7 哈希失配，帧内首失配偏移均为 5，绝对偏移
  7537 / 16753；帧 3–11、7–11 沿依赖链全部级联为不可信，共 9 帧受影响。
- 增量语义：全新工作目录首跑 `recomputed=12 reused=0`，二跑
  `recomputed=0 reused=12`。
- 版本升级：v1→v2 输出 6 条锚点差异（范围/水印/参考类型），隐式迁移
  清单被 `E_MANIFEST_VERSION` 拒绝。
- 恢复：人为破坏快照 `recordHash` 后 `resume` 返回
  `E_RECOVERY_INVALID`，旧快照文件保持不变。
- 并发：重复提交返回 `OK then E_BUSY`，取消后仍可读取已发布快照。
- 确定性：同种子重新生成样本，三组轨迹日志 `cmp` 逐字节一致。
- 报告一致性：JSON 诊断报告与轨迹日志的帧号、SHA-256、判定、级联状态
  经脚本交叉校验全部一致。
