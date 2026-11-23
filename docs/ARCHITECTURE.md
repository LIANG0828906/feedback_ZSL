# 设计与实现说明

## 1. 模块划分

```text
cmd/gaitretarget/       命令行入口
internal/gait/
  generate.go           固定种子样本与真值生成
  validate.go           数据契约、骨架、相位、异常校验
  mapping.go            周期对齐、帧映射、占比检查
  solve.go              相位驱动足部约束、腿部 IK、事件与校正
  engine.go             周期状态机、并行调度、提交顺序
  checkpoint.go         原子快照、哈希与整体校验
  compare.go            首个差异定位
  verify.go             全量/实时/断点恢复三路径验收
```

数据只通过本地 JSON 文件输入输出。`go.mod` 没有 require 项。

## 2. 相位判定与映射

单足判定：

```text
swing = (height > swing_height_min)
     or (height > contact_height and horizontal_speed > swing_speed_min)
stance = not swing
```

先判高度可避免落地帧因接近速度较大而被误判为摆动。全局相位：

```text
双足 stance       -> double_support
左足 swing        -> swing
其余              -> stance
```

源周期到目标周期的映射步骤：

1. 读取目标声明周期长度 `targetLen`。
2. 检查 `targetLen ≈ sourceLen * leg_length_ratio`，允许 1 帧取整误差。
3. 用端点对齐的最近邻重采样生成 `source_frame_map`。
4. 以源帧相位生成目标逐帧相位。
5. 比较 `stance/swing/double_support` 占比，超过容差返回 `E_ALIGN`，冲突项同时带源侧和目标侧占比。

`Mapping.QueryCycle` 返回周期边界、缩放、分段、占比和帧映射；`Mapping.QuerySourceFrame` 可查询任意源帧对应的目标帧。

## 3. 足部与腿部求解

支撑期：

- `Y = target.ground_plane`；
- 每个连续支撑段固定到首帧 X/Z 锚点；
- 输出水平速度为零，天然低于 `stance_slide_max`。

摆动期：

```text
target_lift = source_lift * leg_length_ratio
            * (1 + 0.02 * (toe_count - 1))
            * swing_lift_scale
```

若 `target_lift` 超出目标 `[foot_reachable.min, foot_reachable.max]`，返回 `E_HEIGHT`，并带目标帧号和足部 ID。

腿部链：

- 髋位置 = 根平移 + 髋局部偏移；
- 膝、踝长度来自 T 姿态局部偏移；
- 用双骨平面 IK 计算膝关节；
- 踝角按目标 `ankle_rom` 截断；
- 每个目标趾关节输出位置和角度，数量严格等于目标骨架声明。

根轨迹由源根平移按腿长比例缩放；若足部目标超出腿长可达范围，根高度按周期内最紧约束下压，保持输出关节链可复现。

## 4. 相位复判与一次局部校正

求解后的目标足部位置再次用高度/速度阈值分类。若聚合相位与映射相位不同：

- 当前帧只校正一次；
- 输出中使用重新检测的相位；
- `Correction` 记录 `frame`、`cycle`、`foot`、`reason`、`mapped_phase`、`detected_phase`、`corrected_phase`。

正常样本校正数为 0；测试会构造一个可合法通过相邻相位校验、但足部仍双支撑的帧，验证校正记录和三路径一致性。

## 5. 事件时间轴

每个周期独立生成事件。周期首帧使用源上一帧相位作为“周期前接触状态”，因此跨周期落地事件归属新周期，且不需要读取前一周期计算结果。

`land` 表示 `swing -> stance`，`liftoff` 表示 `stance -> swing`。真值事件源帧经 `Mapping.QuerySourceFrame` 映射后，要求目标事件在 `landing_tolerance_frames` 内出现。

## 6. 并发与提交

引擎按周期号取批，每批最多 `max_parallelism` 个 goroutine。每批完成后按周期号顺序：

1. 失败周期从 `solving` 进入 `failed`；
2. 成功周期写入检查点；
3. 检查点成功后才进入 `committed`。

因此工作线程完成顺序不会影响提交顺序，也不会影响最终结果。单周期失败不阻止同批或后续周期提交。

`cycle_buffer_limit` 在周期映射和求解前校验，避免为超长周期分配无界缓冲。

## 7. 检查点完整性

内容哈希计算的是：

```text
hash(canonical_json(snapshot_without_content_hash))
```

快照同时携带：

- `input_hash`：源、相位、目标、参数整体哈希；
- `parameter_version`；
- `algorithm_version`；
- `content_hash`。

写入使用同目录临时文件，`fsync` 后原子 rename，再由存储互斥锁保证不交错。加载逐文件校验；任一文件不合法即删除所有周期快照，禁止部分复用。

## 8. 三路径一致性

- **全量**：`FullRetarget` 顺序计算所有周期，不写检查点。
- **实时**：引擎按并行批次计算，每周期提交并写检查点。
- **断点恢复**：第一个引擎推进指定周期数后退出；第二个引擎加载快照，只计算未提交周期。

三条路径使用同一单周期纯函数求解器。引擎结果出口会再次校验真值事件容差。`CompareResults` 按帧序比较并定位首个差异，包括相位、根轨迹、足部、关节、事件和校正记录。
