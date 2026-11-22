# EdgePipe 设计说明

## 1. 数据流

```text
输入图像
  └─ 分块网格（固定 block_size，边缘块可小于 block_size）
      └─ 阶段屏障：gradient → open → close → thin → prune
          ├─ 每个块带 overlap band 计算扩展块
          ├─ 仅提取核心区写回本阶段全局图
          ├─ 每块阶段结果原子提交检查点
          └─ 本阶段全部完成后再进入下一阶段
```

阶段屏障是检查点提交顺序和跨块一致性的基础。全局提交序列为：

```text
stage-major, block-ascending:
gradient/b0000, gradient/b0001, ..., open/b0000, ..., prune/bNNNN
```

计算可以由 `max_workers` 个线程并行，但协调器按上述顺序逐个提交，因此检查点不会因线程完成顺序不同而产生不同序列。

## 2. 五阶段算子

### 2.1 梯度与双阈值

支持 Sobel、Scharr 与 Prewitt。对每个通道独立计算：

```text
magnitude = sqrt(gx² + gy²)
strong    = magnitude >= high_threshold
weak      = magnitude >= low_threshold
edge      = strong OR (weak AND dilate(strong, 3×3))
```

这里使用“局部一像素滞后”而不是 OpenCV Canny 的全局递归滞后。原因是全局滞后可能沿边缘无限传播，使有限重叠边带无法证明分块结果等价；局部规则支持半径固定，便于严格边界一致性。

输出为 `uint8` 的 `0/255`，保持输入高度、宽度和通道数。

### 2.2 开运算与闭运算

使用方形结构元，分别执行 `MORPH_OPEN` 与 `MORPH_CLOSE`，迭代次数可独立配置。结构元尺寸必须为 1..31 的奇数。尺寸 1 表示恒等结构元，用于保留很薄的梯度边缘。

### 2.3 细化

自实现 Zhang–Suen 两子迭代，每轮执行两次子遍历，最多迭代 `thinning_iterations` 次；提前稳定时停止。自实现避免依赖 `opencv-contrib`，并让每轮依赖半径明确为 1 像素。

### 2.4 短线段剪枝

对每个 8 连通分量计算包围盒直径：

```text
diameter = hypot(width - 1, height - 1)
```

仅删除 `diameter <= prune_length` 的分量。该规则保留长线，同时保证分块判定所需上下文不超过 `prune_length`。

## 3. 重叠边带与边界一致性

配置默认边带：

```text
required_band =
  max(
    1,
    (open_kernel_size // 2) * open_iterations,
    (close_kernel_size // 2) * close_iterations,
    thinning_iterations,
    prune_length
  ) + 1
```

各项来源：

- 形态学每轮依赖结构元半径；
- Zhang–Suen 每轮向邻域传播 1 像素；
- 剪枝需要看到完整短分量的最大跨度；
- `+1` 覆盖局部梯度滞后与边界安全余量。

每个块实际处理扩展裁剪区域，计算后只把核心区写回全局阶段图。裁剪区域在图像边缘处直接截断；由于整图算子在该边缘也看到相同的“图像外为空/默认边界”，所以边缘块与整图语义一致。

如果用户显式设置 `band_width < required_band`，配置阶段立即返回 `E_BAND`，不会通过裁剪核心区或跳过边界像素规避差异。

## 4. 四态状态机

每个 `(block_id, stage)` 初始为 `PENDING`。合法迁移：

```text
PENDING    -> PROCESSING
PROCESSING -> COMMITTED
PROCESSING -> FAILED
PROCESSING -> PENDING      # 协调器在未提交批次被放弃时的回滚
FAILED     -> PROCESSING   # 重试
```

`COMMITTED` 是终态。非法迁移返回 `E_STATE` 且不修改当前状态。批次中某个块异常时：

- 抛出异常的块变为 `FAILED`；
- 同批已计算但尚未提交的后续块回滚为 `PENDING`；
- 已提交块和已完成阶段不回滚、不改写；
- 下一次 `resume`/`incremental` 从 manifest 的连续前缀继续。

进度定义为：

```text
committed / (num_blocks × num_stages)
```

## 5. 检查点协议

检查点目录包含：

```text
snapshot.json
blocks/
  0000_b0000_gradient.npz
  ...
```

每个 blob 保存：

- `mask`：该块该阶段的核心输出；
- `meta`：提交序号、分块/阶段标识、内容哈希、参数版本、算法版本、形状、dtype。

`snapshot.json` 保存完整配置、输入哈希、参数版本、算法版本、分块布局、阶段列表、下一提交序号和全部 entry。

提交顺序：

1. 写唯一临时 blob，`fsync`，`os.replace` 原子替换；
2. 重新序列化完整 manifest，写临时文件，`fsync`，`os.replace`；
3. 更新内存 manifest。

manifest 是唯一提交点。若在步骤 1 和 2 之间崩溃，新 blob 是孤儿，重启时被忽略；下一次同序号提交可安全覆盖该孤儿临时目标。所有提交由协调器串行调用，store 内部另有可重入锁。

启动校验：

- schema、输入哈希、参数版本、算法版本；
- 分块布局、阶段列表、总提交数；
- entry 是否从 0 开始连续、是否符合阶段优先/分块升序；
- 每个 blob 是否可读、形状/dtype 是否匹配、meta 是否与 manifest 一致；
- 重新计算内容哈希。

任一项失配：整份快照判废，清空后全量重算，禁止只复用部分旧块。

## 6. 三条路径

| 路径 | 启动状态 | 行为 |
|---|---|---|
| `full` | 清空旧检查点 | 全部提交重新计算 |
| `incremental` | 校验现有快照 | 复用连续已提交前缀，只计算未提交任务 |
| `resume` | 校验现有快照 | 与 incremental 相同的恢复引擎；用于崩溃/退出后的新进程 |

验收器会：

1. 全量路径完整运行；
2. 增量路径先优雅停止在 7 个提交，再由新引擎实例以 `incremental` 完成；
3. 恢复路径在 7 个提交后模拟崩溃，再由新引擎实例以 `resume` 完成；
4. 比较五阶段掩膜、最终掩膜、几何统计、提交序列和状态矩阵；
5. 再与整图单块参考逐阶段比较；
6. 复制完整快照并注入四类版本/哈希失配。

## 7. 几何真值

样本真值包含：

- 线段端点 `(x1, y1) -> (x2, y2)`；
- 圆中心 `(cx, cy)` 与直径。

统计方式：

- 线段：等距采样真值点，计算到最近掩膜像素距离、覆盖率、边缘支持像素的法向平均偏移；
- 圆：在期望半径附近选择支持像素，做代数最小二乘圆拟合，统计圆心位移、半径误差和 72 个角度 bin 的覆盖；
- 总体最大系统位移与各覆盖/拟合阈值决定 `all_pass`。

这能区分“掩膜有边缘但整体偏移”和“无系统性位移但边缘有局部断裂”。

## 8. 资源与并发

- `max_workers` 限制为 1..4；
- 每批最多提交 `max_workers` 个计算任务；
- `thread_buffer_bytes` 限制单线程保守工作集估算；超过时返回 `E_PARA`；
- 线程只计算，不写检查点；协调器单线程按序提交；
- 已提交条目没有覆盖式更新接口。

## 9. GUI

GUI 通过同一 `PipelineEngine` 在 `QThread` 中执行，不另写一套算法。状态下表直接展示引擎的四态矩阵；点击单元格显示 attempts/error；失败/待处理任务可触发 `resume` 重试。差异热力图来自当前分块最终掩膜与整图单块参考的绝对差。

## 10. 验收映射

| 业务要求 | 实现/验证 |
|---|---|
| 梯度、双阈值、通道/尺寸 | `ops.gradient_threshold`；`test_ops.py` |
| 开关闭/细化/剪枝、尺寸一致 | `ops.apply_stage`；阶段尺寸测试 |
| 四态状态机与重试 | `states.py`；状态/失败重试测试 |
| 原子检查点与版本失效 | `checkpoint.py`；哈希/参数失效测试 |
| 三路径逐像素一致 | `verify.py`；`test_three_paths_diff.py` |
| 边带与跨块一致 | 边带公式；分块 vs 整图测试 |
| 解码/状态/参数/边带/差异异常码 | `errors.py`；`docs/VALIDATION.md` 异常码表 |
| 强制终止恢复 | CLI `os._exit` 子进程测试 |
| 可视化与资源边界 | `gui.py`；offscreen GUI 测试 |

