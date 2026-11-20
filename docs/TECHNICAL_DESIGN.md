# 多版本骨骼动画重定向管线 — 技术设计

## 1. 目标与范围

管线在本地把 v1/v2/v3 三种绑定格式的同源骨骼动画重定向到统一目标骨架，
覆盖解析、映射、版本兼容、绑定姿态对齐、重定向、一致性校验、快照恢复、
后台并发控制与 PyQt6 预览，全部样本由确定性生成器产出，不访问外部资源。

## 2. 模块架构

| 模块 | 职责 | 对应业务要求 |
| --- | --- | --- |
| `errors.py` | 业务码 `E_*`、带 joint/field/frame 定位的异常 | 1–7 |
| `math3d.py` | 四元数、旋转矩阵、层级 FK、拓扑成环检测 | 1/4/5 |
| `model.py` | `JointDef`/`AnimDocument`/`RetargetResult` 数据模型 | 全部 |
| `codec.py` | v1/v2/v3 严格解析、序列化、原子写、规范化字节 | 1/8 |
| `naming.py` | 关节名规范化、层级路径 | 2/3 |
| `mapping.py` | 别名→规范化名→路径匹配、冲突、最近祖先回退 | 2 |
| `compat.py` | v1/v2 升级 v3、首帧推导绑定姿态、降级损失报告 | 3 |
| `retarget.py` | 绑定姿态差对齐、全局空间重定向、逐帧生成器 | 4 |
| `validate.py` | 源真值轨迹比对、偏差统计、跨版本一致性 | 5 |
| `snapshot.py` | 工程文件、快照哈希链、原子提交、崩溃恢复 | 6 |
| `engine.py` | 后台线程、单任务锁、`E_BUSY`、协作式取消 | 7 |
| `samples.py` | 确定性样本、别名/缩放/近重合旋转 | 8 |
| `trajectory.py` | 帧级轨迹日志与内容哈希 | 8 |
| `gui.py` | 时间轴播放、映射表、偏差热力 | 8 |

## 3. 文件格式

统一为 UTF-8 JSON，顶层 `format_version` 取值 1/2/3：

* v1：`joints[].name`、`frames[].rotations`（四元数 `[w,x,y,z]`）；
* v2：增加 `joints[].parent`（根为 `-1`）、`frames[].translations`；
* v3：增加 `joints[].bind_matrix`（4x4）、`joints[].scale`、`joints[].aliases`。

解析采用“全量校验后才构造文档”，任一错误抛 `E_ANIM_FORMAT_INVALID`，
`locations` 精确定位到关节名、字段与帧号，不会部分加载。序列化用
排序键 + 固定缩进，`canonical_bytes` 支撑内容哈希与逐字节复现。

## 4. 关节映射

匹配优先级：**v3 别名表 → 规范化名 → 根到自身层级路径**。

* 冲突：多个源关节指向同一目标时，确定性保留首个来源，其余写入
  `conflicts`，业务码 `E_MAP_CONFLICT`，不静默覆盖；
* 未匹配源关节进入 `unmapped_sources`；
* 未被命中的目标关节回溯“最近已映射祖先目标”，标记 `ancestor_fallback`；
* 映射结果可 JSON 序列化，`content_hash` 作为重定向与快照的确定性输入。

## 5. 版本兼容

* 升级：v1 线性链推导父子关系；绑定姿态缺失时由首帧局部旋转+平移构造
  `bind_matrix`，标注 `derived:first_frame`；别名缺失按规范化名生成；
* 降级：`plan_downgrade` 先计算字段损失（`scale`/`bind_matrix`/
  `aliases`/`parent`/`translations`）与受影响关节，默认直接抛
  `E_VERSION_LOSS`；显式 `accept_loss=True` 才导出并把损失清单写入日志。

## 6. 重定向数学

四元数布局 `[w,x,y,z]`。对源、目标分别做 FK 得到全局绑定朝向，
逐目标关节计算绑定姿态差

```
align_i = bindTgt_i * inverse(bindSrc_i)
```

逐帧目标全局朝向

```
T_global_i = align_i * S_global_i * inverse(align_parent_i)
```

再按目标层级反算局部朝向；根位移取源根全局位移经根对齐旋转后，
**仅乘一次目标根缩放**。v3 骨骼缩放只烘焙进绑定偏移/根位移，
旋转级联始终为纯四元数，杜绝父子重复缩放。矩阵经 SVD 极分解投影到
最近合法旋转；退化时该帧标记 `E_POSE_UNRESOLVED`，保留在内存结果但
跳过快照落盘。

## 7. 一致性校验

以源骨架逐帧全局朝向轨迹为真值：用同一对齐公式求目标期望朝向，
与重定向输出逐关节逐帧比对测地角；同时把输出局部姿态重新 FK 检查
自洽性。报告含均值/RMS/最大偏差、最大偏差关节、超差帧号+关节。
`compare_version_runs` 比较 v1/v2/v3 升级后运行的最大偏差差量，
超容差以 `E_CONSISTENCY_FAIL` 报告版本对。

## 8. 快照与恢复

* 写入统一走临时文件 + `fsync` + `os.replace` 原子替换；
* 快照体不含自身哈希，落盘后复算 sha256 写入 `content_hash`，
  `prev_hash` 链接前一快照形成哈希链；
* 恢复逐链复算哈希并校验 `prev_hash`，断链/指针不一致抛
  `E_RECOVERY_INVALID`；
* 传入当前 `mapping_hash` 与格式版本，变化时返回 `needs_recompute`
  及原因（`mapping_hash_changed`/`source_version_changed`/
  `target_version_changed`），触发重算而非复用旧结果。

## 9. 并发与取消

`RetargetEngine` 为每个动画键维护一把锁，重复提交立即返回 `E_BUSY`。
工作线程消费逐帧生成器，**取消只在帧边界检查点生效**；单帧内部的
矩阵/四元数求解为不可中断步骤，取消不会谎称已终止。取消或异常后
`with` 自动关闭文件句柄，已提交快照完整保留。

## 10. 确定性

随机源仅为固定种子的 `numpy.random.RandomState`；JSON 排序键、固定
缩进、四元数归一化与确定性冲突策略保证相同种子+操作序列下，样本、
重定向动画与轨迹日志逐字节一致（见验证报告）。

## 11. Windows 桌面运行

代码仅依赖 Python 标准库、NumPy、PyQt6，路径均用 `os.path` 拼接，
无容器/虚拟化假设。Windows 本地有显示时直接 `python scripts/cli.py gui ...`
进入事件循环；无显示环境可用 `--offscreen` 或设置
`QT_QPA_PLATFORM=offscreen` 做界面构建自检。
