# Motion Workbench 技术说明

## 1. 设计目标

系统把“求解”和“提交”分开：

1. 求解器只处理内存中的单帧姿态。
2. 只有完整收敛的连续帧才能形成区间提交。
3. 提交后的区间文件不可变，后续重解通过新的序号和内容哈希追加版本。
4. 恢复只读取 `source.bvh`、当前参数快照、`manifest.json` 和区间文件，不依赖进程内状态或退出报告。

核心计算不依赖第三方库，因此解析、求解、提交和恢复可以在无 NumPy 的环境中进行回归测试；目标 Windows 环境仍按 `requirements.txt` 安装指定版本。

## 2. BVH 解析与 FK

### 2.1 层级

解析器支持标准 `ROOT`/`JOINT`/`End Site`、`OFFSET`、`CHANNELS`、`MOTION`、`Frames:` 和 `Frame Time:`。为保证父引用可验证，额外支持可选形式：

```text
JOINT child PARENT 3
```

显式父索引必须在关节表范围内，并与实际嵌套父节点一致。普通标准 BVH 不写 `PARENT`，解析后仍会重新校验所有隐式父索引、父节点先于子节点的拓扑顺序。

### 2.2 错误定位

`WorkbenchError.details` 始终包含：

```json
{
  "joint": "effector",
  "joint_index": 7,
  "frame": 42
}
```

缺失值使用 `null`。通道数量不符时会再附 `expected_channels`、`actual_channels` 和 `channel_index`；声明帧数缺失时附 `expected_frames`、`actual_frames`。

### 2.3 确定性

- 关节顺序固定为层级深度优先。
- 通道顺序完全按文件声明顺序解释。
- 帧值只在解析时转换为 `float`，后续不排序、不重新采样。
- 内容摘要基于关节表、通道、偏移、帧值和 `frame_time` 的规范 JSON。
- 同一文件重复解析得到相同的关节元组、帧元组和 `content_digest`。

FK 使用四元数局部旋转：

```text
world_rotation[j] = world_rotation[parent] * local_rotation[j]
world_position[j] = world_position[parent]
                    + world_rotation[parent] * (local_translation[j] * scale[j])
```

`scale[j]` 是求解器保留骨骼长度约束时使用的沿父到当前关节的伸缩变量。

## 3. 约束与残差

每帧输出每个约束的：

- `constraint_id`
- `type`
- `joint`
- `raw`
- `tolerance`
- `weight`
- `normalized = raw / tolerance`
- `satisfied`
- `evidence`

总残差 `total_residual` 是已启用约束的加权归一化 L2 范数：

```text
sqrt(sum(weight_i * (raw_i / tolerance_i)^2))
```

`weight == 0` 表示约束禁用：残差仍报告，但不参与目标和冲突判定。

三类证据字段：

- 骨骼长度：`actual_length`、`target_length`、`scale`
- 关节限位：`rotation_order`、`euler_deg`、`min_deg`、`max_deg`、`violation_axis`、`violation_deg`
- 末端目标：`target`、`actual_world`、`distance`

求解采用确定性块坐标下降：按关节深度排序，对局部三轴旋转和祖先链尺度做固定顺序的有限步长试探；无改进时减半步长，迭代上限固定。该算法不使用随机数或墙钟时间，因此相同输入得到相同姿态与哈希。

## 4. 显式影响闭包

### 4.1 关节链

参数变化时：

- 骨骼长度/关节限位：受影响关节为变化关节、其祖先和整棵子树，因为局部旋转变化会传播到后代，而满足约束也可能需要祖先调整。
- 末端目标：受影响关节为目标关节及其祖先。
- 全局迭代/步长参数变化：所有帧，原因标记 `global_solver_parameters_changed`。

### 4.2 帧传播

- 约束权重/容差变化：使用该约束旧/新帧范围的并集。
- 末端目标值变化：只使用目标值实际变化的帧。
- 新增/删除约束：使用该约束帧范围。
- 关节链受影响后，当前末端约束的目标帧与已变化帧集合求交，作为闭包补充。
- 若关节/依赖无法解析，退化为所有帧，`conservative=true`，原因 `unresolved_joint_dependency`。

轨迹中的 `impact_plan` 事件记录：

- `recomputed_ranges`
- `changed_constraints`
- `affected_joints`
- `reasons`
- `conservative`
- 每个变化的详细帧范围和关节

求解不会使用固定全量重解兜底。

### 4.3 缓存有效性

每个帧提交时保存 `dependency_hash`，它只包含该帧实际启用的约束、有效目标和全局求解参数。恢复/增量时逐帧重算依赖哈希：

- 哈希相同：复用已提交姿态、残差和 `frame_hash`。
- 哈希不同：加入重解集合。

因此无关约束变化不会使旧帧失效，而影响帧的求解仍从 BVH 基准姿态开始，保证与全新全量求解一致。

## 5. 磁盘格式

会话目录：

```text
session/
  source.bvh
  manifest.json
  parameters/<parameters_version>.json
  intervals/v000001_f000000-f000005_<hash>.json
  conflicts/frame_000023_<hash>.json
  trace.jsonl
  .solve.lock
```

### 5.1 参数快照

`parameters_version` 由以下内容哈希得到：

```json
{
  "engine": "motion-workbench/1.0.0",
  "bvh_hash": "...",
  "constraints": {}
}
```

参数文件放在以哈希命名的路径中，内容自校验。更新参数时先写新版本文件，再原子更新 manifest 指针；两步之间强杀只会留下不可达的孤立参数文件。

### 5.2 区间文件

区间内容包含：

- `start`、`end`、`sequence`、`table_version`
- `parameters_version`、`bvh_hash`
- 每帧 `pose`、逐约束残差、总残差、`dependency_hash`、`frame_hash`
- `content_hash`
- `previous_hash`、`chain_hash`

`content_hash` 覆盖除自身、前驱和链哈希以外的规范 JSON。`chain_hash` 再绑定 `previous_hash`、`content_hash`、参数版本、区间表版本和帧范围。

区间文件使用同目录临时文件写入，完成后 `fsync`，再 `os.replace` 到最终路径，并同步目录。manifest 最后原子替换。任何一步失败返回 `E_COMMIT_FAILED`；旧 manifest 和旧区间保持有效。

### 5.3 版本与不可改写

每次提交分配新的 `sequence` 和 `table_version`，文件名包含内容哈希。旧的已提交文件从不原地改写。后续重解覆盖某帧时，叠加顺序由更晚的 `table_version` 决定，旧文件仍可用于审计。

### 5.4 Barrier

冲突帧不提交姿态。系统写独立冲突报告，然后向 manifest 追加 barrier：所有更早提交中 `frame >= barrier.frame` 的帧从有效叠加视图中排除。修正参数后的新提交 `table_version` 大于 barrier，因此可重新覆盖该区域。

## 6. 恢复判定

`resume`/`verify` 只从磁盘执行：

1. 读取 manifest，校验 schema、BVH 哈希和参数版本指针。
2. 读取并重算参数快照内容哈希，重新解析约束。
3. 校验参数版本历史，确保每个区间引用的参数版本出现过。
4. 校验区间表事件连续且无重复。
5. 按提交顺序重建前驱哈希链，重算区间 `content_hash` 和 `chain_hash`。
6. 逐帧反序列化，重算 `frame_hash`，拒绝冲突或未收敛帧。
7. 校验 barrier 报告的内容哈希、帧号和报告 ID。
8. 计算有效帧叠加、缺失帧和逐帧依赖哈希是否滞后。

任一失败返回 `E_RECOVERY_INVALID`，`details.first_break_frame` 指向：

- 区间内容/链哈希错误：该区间起始帧。
- 单帧哈希错误：具体帧。
- 链头或参数版本错误：首个受影响帧或 0。

不会返回任何未经校验的姿态。

## 7. 并发与取消

- 进程内注册表阻止同一锁路径被两个线程同时持有。
- POSIX 使用 `flock`，Windows 使用 `msvcrt.locking` 非阻塞独占锁。
- 额外按 BVH `content_digest` 建立全局锁目录，使不同会话目录加载同一 BVH 时也只能有一个求解任务。
- 锁由操作系统在进程终止时自动释放，因此强杀后无需手工清理。
- 每个帧边界检查取消和暂停；取消时先提交当前完整连续帧，再退出并释放锁/文件句柄。
- 强杀可能丢失最后一个未提交 chunk，但所有已提交区间与 manifest 链仍一致。

## 8. 冲突诊断

达到 `max_iterations` 且任一启用残差超过 tolerance 时：

- `status = conflict`
- 不提交冲突帧
- 写 `conflicts/frame_<frame>_<hash>.json`
- 追加 barrier
- 报告包含 `frame`、全部未满足约束、涉及关节、`worst`、`iterations`、`max_iterations`、`evidence`
- 报告含 `report_hash`，可单独验证与定位

## 9. GUI

`motion_workbench/gui.py` 使用 PySide6：

- `SkeletonViewport`：世界空间骨骼透视投影，鼠标旋转/滚轮缩放，关节色由残差热力映射。
- `ResidualHeatmap`：逐帧总残差色带与播放头，可点击跳帧。
- 时间线：播放/暂停循环。
- 侧栏：载入 BVH/约束、会话目录、约束权重/容差表格、启动/暂停/取消/磁盘恢复。
- 求解在工作线程运行，主线程只消费进度和最终报告。

本次 Linux 验证容器没有安装 PySide6，因此完成了 GUI 源码编译和静态结构测试，未在该容器内实际弹出窗口。目标 Windows 环境按固定依赖安装后可运行。

## 10. 验收复现

```powershell
python -m unittest discover -v
python scripts/run_acceptance.py
```

结果写入 `artifacts/verification.json`。脚本覆盖：

- 24 项单元/集成测试
- 样本生成与基线残差摘要
- 单帧末端目标增量与全量逐帧哈希对照
- `os._exit(137)` 强杀后的磁盘链校验和续解
- 冲突报告/barrier
- manifest 写失败与旧状态保持
- 轨迹字段检查
