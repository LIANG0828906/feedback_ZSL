# 点云工作台技术说明

## 模块结构

| 模块 | 职责 | 对应业务要求 |
| --- | --- | --- |
| `pcwb/pcformat.py` | PCWB v1 二进制格式读写、内容哈希 | 1 |
| `pcwb/blocks.py` | 空间分块显示层级、重建差异报告 | 2 |
| `pcwb/lod.py` | 视点驱动 LOD 选择、预算、边界一致性校验 | 3、4 |
| `pcwb/ops.py` | 裁剪/平滑/降采样/颜色覆盖纯函数算子、框选 | 5 |
| `pcwb/store.py` | 版本化存储、原子提交、回退 | 6 |
| `pcwb/runtime.py` | 后台编辑线程与渲染快照隔离 | 7 |
| `pcwb/samplegen.py` | 确定性样本生成器 | 8 |
| `pcwb/demo.py` | 脚本化演示入口（轨迹日志） | 8 |
| `pcwb/glcheck.py` / `pcwb/glrender.py` | 离屏 GL 检查与渲染留痕 | 环境准备 |

## 二进制格式（PCWB v1，小端）

头部 44 字节：`magic="PCWB"`、`format_version=1`、`header_size=44`、
`point_count`(u64)、`bbox_min`/`bbox_max`（各 3×f32）；随后每点 15 字节
（XYZ 3×f32 + RGB 3×u8）。读取时校验 magic、版本、头长、点数与文件长度
一致、坐标落在包围盒内；失败抛 `E_POINTCLOUD_INVALID` 并在 `detail`
中给出 `field`（头部字段名）或 `point_index`。内容哈希 = 点数据区
SHA-256，与文件布局逐字节一致，写盘/切块/显示共用同一统计口径。

## 分块与边界去重

块划分：`cell = floor((p - bbox_min) / edge_length)`。落在共享边界面上的
点由 floor 规则唯一归属一侧块，去重依据点坐标与点序精确判定，不使用
浮点近似阈值。块内点序保持载入顺序（稳定排序），同一版本重复切分
逐字节一致。`rebuild()` 输出新旧块数、总点数差异与被替换块清单。

## LOD 与一致性真值

抽样序列：层级 L 保留 `SplitMix64(seed ^ 全局序号) % 2^L == 0` 的点。
判定只依赖 (seed, 全局序号, 层级)，与切块方式无关，因此分块显示的
抽样并集与整份单块真值逐点一致（`verify_seam_consistency`，超差抛
`E_BLOCK_SEAM` 并定位边界块对）。屏幕误差按块包围盒投影尺寸换算，
超帧预算时整体升档；全部块到最大层级仍超预算抛 `E_LEVEL_BUDGET`
并附各块预算用量 `usage`。可见块至少显示 1 个代表点（哈希最小者）。

## 编辑算子与版本提交

四类算子为纯函数：输入 (points, colors, selection, params)，返回新
数组，不改入参；相同输入输出逐字节一致。空选区、非法参数、输出
超 `max_output_points` 抛 `E_OP_INVALID`，已提交版本不受影响。
提交：临时文件 + `os.replace` 原子重命名，版本号单调递增，记录算子、
参数、输入/输出内容哈希与被替换块清单（`versions/log.jsonl`）；
失败抛 `E_COMMIT_FAILED`，上一版本文件与日志不变。回退把历史版本
内容作为新版本追加，不改写历史。

## 线程隔离

编辑在后台线程执行纯函数并提交；渲染线程通过 `acquire()` 获取句柄
读取不可变快照。新版本发布是一次整体引用替换，替换前渲染读旧快照，
不会观察到部分更新的块；句柄在发布后失效，读取抛 `E_STALE_HANDLE`。

## 确定性

样本由 `RandomState(seed)` 生成，颜色为坐标确定性函数；轨迹日志中的
`frame_cost_us` 为确定性帧耗时模型（`displayed*2 + blocks*50` µs），
真实墙钟耗时仅打印不进日志。相同种子与操作序列下，样本文件、版本
文件、轨迹日志、离屏渲染图均逐字节一致（已实测 diff/cmp 通过）。

## 验证结果（本机实测）

- 环境：Python 3.14.4，NumPy 2.5.5，PyOpenGL 3.1.10，PyQt5 5.15.11；
  离屏 GL 后端 Mesa llvmpipe（软件渲染），GL 4.5。
- 单元测试：`python3 -m unittest discover -s tests`，22 项全部通过，
  覆盖格式校验定位、分块确定性、预算错误、边界一致性、算子纯函数
  性质、原子提交失败回滚、后台编辑与快照隔离、陈旧句柄。
- 演示：`scripts/run_demo.sh`，10 万点碎片（1094 块）与 500 万点整件
  （1323 块）均完成巡览、四类算子提交与回退；分块显示与单块真值在
  层级 0/1/2 下逐点一致；连续两次运行 `trace.log`、全部版本文件与
  渲染图逐字节一致。轨迹见 `out/trace.log`。
