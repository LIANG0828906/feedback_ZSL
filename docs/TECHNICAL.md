# 流式分块预览与误差归因模块

## 1. 交付范围与后端选择

本仓库提供一个无第三方运行时依赖的 C++17 模块：

- 自定义场景二进制解析、引用校验和包围体层级构建。
- 后台分块加载、优先级/距离调度、不可变加载快照发布。
- 保守视锥剔除、遮挡体软件光栅化、逐层深度金字塔。
- 逐层遮挡查询、临界判定、误差预算与帧摘要。
- 分块 ID 屏幕可见性掩码、快照/金字塔版本绑定。
- 原子快照持久化、损坏检测、视点/加载集合变化识别和 SIGKILL 恢复。
- 确定性样本生成器、基线哈希、JSONL 轨迹日志。

离屏后端使用确定性的 CPU 软件光栅化参考实现。当前验收环境没有可用的
OpenGL 4.3 开发头或离屏驱动，因此测试不依赖 GPU。深度金字塔构建、遮挡查询、
掩码合成和快照边界均已按可替换后端拆分；接入 GPU 后端时不需要改变业务码、
格式、版本隔离或误差摘要接口。

## 2. 目录

```text
include/preview/      公共 API 与数据结构
src/                  解析、流式加载、剔除、金字塔、误差、渲染、快照实现
tools/scene_gen.cpp   场景/视点/基线/轨迹生成器
tools/preview_cli.cpp 渲染、快照恢复、崩溃恢复演示
tests/                格式、并发、渲染、确定性和崩溃恢复测试
scripts/verify.sh     一键构建与验收脚本
```

## 3. 场景二进制格式

所有整数和小端 IEEE-754 浮点均显式编码。场景头 40 字节：

| 偏移 | 类型 | 字段 |
|---:|---|---|
| 0 | `u8[8]` | `ZSPVSCN1` |
| 8 | `u32` | 格式版本，当前为 1 |
| 12 | `u32` | flags |
| 16 | `u32` | 分块数 |
| 20 | `u32` | 根表项数 |
| 24 | `u64` | 头后载荷字节数 |
| 32 | `u64` | 场景内容哈希 |
| 40 | - | 根 ID 表，随后是连续分块记录 |

每个分块记录：

```text
u32 id, u32 parent, f32 bounds_min[3], f32 bounds_max[3],
u32 priority, u32 occluder,
u32 child_count, u32 vertex_count, u32 index_count,
u32 children[child_count], f32 vertices[vertex_count][3],
u32 indices[index_count]
```

`parent = 0xffffffff` 表示根分块。父引用和显式子引用表必须互相一致。

解析器执行以下校验，失败返回 `E_SCENE_INVALID`，诊断中带块 ID、字段名和文件偏移：

- magic、版本、载荷长度、计数上限、尾部多余字节。
- 重复/保留块 ID、非有限包围盒、`min > max`。
- 父引用悬空、子引用悬空/重复、父子双向引用不一致。
- 层级环、非根块缺少父引用、根表项悬空。
- 父包围盒不包含子包围盒。
- 索引越界、索引数不是 3 的倍数、非有限顶点。
- 头部内容哈希与实际内容不一致。

## 4. 流式加载

调度顺序为：

1. `priority` 降序。
2. 视点到分块包围盒中心距离平方升序。
3. 块 ID 升序。

`StreamingLoader` 用集合去重队列、在途块和已加载块。后台线程按批次完成加载后
构造新的完整 `LoadSnapshot`，再一次性替换当前指针。读取方获取的是
`shared_ptr<const LoadSnapshot>`，因此替换期间只能看到完整旧集合或完整新集合。

加载快照内容哈希覆盖场景内容哈希、块数量、每个块 ID 与块内容哈希。快照版本只在
完整批次发布时递增。取消/停止时未发布的批次不会进入快照。

## 5. 视锥、金字塔与重建

视锥判定将包围盒 8 个角点变换到裁剪空间，对 6 个裁剪平面取保守边界。只要不能
证明所有角点位于同一平面外侧，就保留该分块。

深度金字塔存储线性视点深度，而非固定 NDC 深度。设：

```text
depthSpan = quantizedDepthMax - near
baseStep  = depthSpan / (2^quantizationBits - 1)
step(L)   = baseStep * 2^L
```

`quantizedDepthMax` 由当前已加载遮挡体的最远视点深度决定，所以视点变化会改变
量化步长。第 0 层以最近深度写入，上层取 2x2 最大深度并重量化。空区域保留远深度
哨兵，避免把无遮挡区域误判为遮挡。

金字塔 key 包含加载快照版本/内容哈希、视点矩阵、近远平面、量化深度范围、基础步长、
分辨率和层数。key 改变必须重建。每次重建输出：

- 新旧金字塔版本、层数、深度上下界。
- 深度范围差。
- 新旧层级的最大量化误差。

## 6. 遮挡查询与临界判定

查询从适合包围盒屏幕覆盖面积的粗层开始，若结论不确定则逐层下降到更细层：

```text
margin = boxNearViewDepth - occluderMaxViewDepth
uncertainty =
    currentLayerQuantizationError
  + bboxTransformError
  + frustumBoundaryError
  + accumulatedLayerSwitchJump

lower = margin - uncertainty
upper = margin + uncertainty
```

- `lower > 0`：`Occluded`。
- `upper < 0`：`Visible`。
- 否则：`Uncertain`，业务码 `E_OCCLUSION_UNCERTAIN`。

因此只要余量区间跨过当前层量化误差与包围盒变换误差之和，就不会判为遮挡。
每个临界块携带 `marginLower`、`marginUpper`、所选层号、访问层数和层切换次数。

## 7. 误差传播与帧摘要

每块的误差项包括：

- `bbox_transform_world`：包围盒/相机尺度乘 float epsilon 的保守变换误差。
- `pyramid_quantization_world`：所选层量化步长的一半。
- `layer_switch_jump_world`：逐层下降时余量绝对变化累计。
- `frustum_boundary_world`：视锥边界保守误差。
- `occlusion_upper_bound_max_world`：上述项之和的最大值。

帧摘要包含可见/遮挡/临界/剔除块数、层切换次数、临界块数、各误差项贡献总和、
遮挡误差上界最大值，以及临界块屏幕包围盒面积之和作为掩码偏差像素上界。

`FrameErrorSummary::canonicalJson()` 使用经典 locale 和 17 位有效数字；摘要哈希是
该规范文本的 FNV-1a 64。固定视点、固定参数和同一加载快照下，连续渲染的掩码
像素、摘要文本和摘要哈希逐字节一致。

## 8. 版本隔离

渲染开始时绑定 `shared_ptr<const LoadSnapshot>` 和金字塔版本。后续加载发布不会
改变进行中的集合。掩码记录并校验：

- 加载快照版本。
- 加载集合内容哈希。
- 金字塔版本。
- 掩码像素内容哈希。

调用方可用 `expectedSnapshotVersion` 要求指定版本；当前版本不一致立即返回
`E_SNAPSHOT_STALE`。如果新快照在渲染过程中发布，渲染仍以旧快照完成，并在结果中
设置 `newerSnapshotAvailable = true`，调用方可重新调度。

`validateVisibilityMaskBinding` 可独立检查已有掩码是否仍绑定到给定加载快照和
金字塔；版本不一致返回 `E_SNAPSHOT_STALE`，像素被破坏返回
`E_SNAPSHOT_CORRUPT`。

## 9. 渲染快照与恢复

快照文件头为：

```text
u8[8] magic = "ZSPVSNP1"
u32 container_version
u32 reserved
u64 payload_size
u64 payload_checksum
payload...
```

载荷保存视点与视点矩阵、加载快照版本/内容哈希、已加载块 ID 与内容哈希、金字塔
层尺寸和量化步长、金字塔内容哈希、掩码尺寸/哈希，以及完整误差摘要。

保存流程为先写 `path.tmp`，刷新关闭后再原子替换目标文件。载入时校验容器版本、
载荷长度、FNV-1a 哈希、加载集合哈希、摘要版本关联和视点矩阵/相机字段一致性。
当前视点或加载集合与快照不同则返回 `requiresRerender = true`，并列出变化类别
`view`、`loaded_set`、`load_content_hash`。

测试会在子进程渲染并原子提交快照后立即发送 `SIGKILL`；父进程只读取磁盘快照
即可完成校验和恢复，不依赖子进程退出报告。

## 10. 并发、取消与资源边界

忙碌标记按场景内容哈希在进程内共享。同一场景即使由多个 `Renderer` 实例操作，
同一时刻也只允许一个任务；重复同步/异步提交返回 `E_BUSY`。

取消标志在以下检查点轮询：

- 加载快照绑定后。
- 金字塔每层初始化、每批遮挡体光栅化、每层下采样。
- 每批分块查询。
- 掩码合成前后、摘要提交前。

取消返回 `E_CANCELLED`，不覆盖最后一次成功提交的快照文件。所有线程、缓冲、
流和文件句柄均由 RAII 释放。

## 11. 样本、基线与轨迹

`scene_gen` 生成确定性的三层层级：根、每深度层分组、叶子块。叶子包含完整立方体
顶点/索引，前层轻微重叠以形成遮挡墙；遮挡密度和随机种子可调。

```bash
./build/scene_gen --out /tmp/demo \
  --grid-x 7 --grid-y 5 --layers 4 \
  --occluder-density 0.7 --seed 42 \
  --views 4 --width 192 --height 108
```

输出：

- `scene.bin`：二进制场景。
- `views.txt`：视点序列。
- `baseline.json`：每个视点的掩码哈希、摘要哈希、快照/金字塔版本。
- `trajectory.jsonl`：视点、误差摘要、层切换、临界块、内容哈希。

`preview_cli render` 输出 `trace.jsonl` 和每个视点的二进制 `view_xxx.mask`。掩码文件
包含 magic、尺寸、加载快照版本、金字塔版本、加载内容哈希、掩码哈希和
`u32` 像素数据（0 为空，否则为块 ID + 1）。

## 12. 构建与验收

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build -j
ctest --test-dir build --output-on-failure
bash scripts/verify.sh
```

本次实际验证结果：

- 常规构建：4/4 CTest 通过。
- ASan + UBSan 构建：4/4 CTest 通过。
- 相同参数生成两次：`scene.bin`、`baseline.json`、`trajectory.jsonl` 逐字节一致。
- 端到端样本（145 块、4 视点）：每帧均输出可见/遮挡计数、误差贡献、加载哈希、
  `mask_hash` 和 `summary_hash`；示例视点得到 73 可见、72 遮挡。
- 同一视点连续两次渲染：掩码像素数组和规范化摘要逐字节一致，第二次复用金字塔。
- 加载推进期间的渲染：结果中快照版本、块数和加载哈希保持旧值，
  `newerSnapshotAvailable = true`；随后使用旧期望版本返回 `E_SNAPSHOT_STALE`。
- 同一场景两个渲染器并发提交：第二个返回 `E_BUSY`。
- 取消后：快照文件字节不变，仍可通过恢复校验。
- 子进程 `SIGKILL`：父进程成功从磁盘快照恢复，`requires_rerender = false`。

## 13. 已知边界

- 当前交付使用 CPU 软件离屏参考后端；GPU OpenGL 4.3 后端未在本环境编译或验证。
- 快照保存掩码哈希和渲染元数据，不保存掩码像素。需要恢复像素时，应读取独立
  `.mask` 文件或为快照增加掩码载荷。
- 跨编译器/架构的浮点舍入不承诺位级一致；确定性承诺针对同一本地构建和样本。
