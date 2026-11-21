# 骨骼动画重定向管线 — 技术说明

## 总览

本管线把外部交付的 v1/v2/v3 JSON 骨骼动画重定向到目标骨架，链路为：
绑定姿态对齐 → 关节映射 → 逐帧全局变换转换 → 骨骼长度缩放补偿 →
局部旋转回算。浮点（float64）与定点（可配置小数位）双路径并行计算，
每一步的数值误差显式传播并产出可定位的归因报告。

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `retarget/formats.py` | v1/v2/v3 解析、校验与版本升级（`E_ANIM_INVALID`） |
| `retarget/mapping.py` | 源→目标关节映射（别名 > 规范化名 > 层级路径，`E_MAP_CONFLICT`） |
| `retarget/pipeline.py` | 双路径重定向计算、量化记录、误差传播、缩放去重（`E_SCALE_APPLIED`） |
| `retarget/checkpoint.py` | 帧区间检查点、哈希链、原子提交（`E_RECOVERY_INVALID`） |
| `retarget/runner.py` | run / resume / finalize 编排与一致性校验 |
| `retarget/samplegen.py` | 确定性样本生成器与基线偏差摘要 |
| `retarget/cli.py` | 脚本化入口（generate / run / resume / finalize） |
| `scripts/acceptance.py` | 一键验收（含 SIGKILL 恢复验证） |
| `tests/test_pipeline.py` | 26 个单元测试（stdlib unittest） |

## 格式与升级约定

- v1：`joints` + `tracks.rotations`。升级默认：扁平层级（全部挂在虚拟根下）、
  无平移、单位绑定姿态、缩放 1.0、无别名表。
- v2：增加 `hierarchy`（joint→parent，`null` 为根）与 `tracks.translations`。
  升级默认：单位绑定姿态、缩放 1.0、无别名表。
- v3：增加 `bind_pose`（每关节 16 元素行主序 4x4 全局绑定矩阵）、
  `bone_scales`、`aliases`（源关节→目标关节名）。
- 所有版本可选 `quat_norm_tolerance`（默认 1e-3）。四元数模长偏离超阈值、
  字段缺失、父引用悬空、层级环均返回 `E_ANIM_INVALID`，定位首个出错关节与
  帧号，且不产出任何中间产物（解析发生在检查点初始化之前）。

## 关节映射

优先级：v3 别名表 → 规范化关节名（小写、去非字母数字）→ 规范化层级路径。
每条映射记录 `method`（alias/name/path）与 `confidence`（high/medium/low）。

- 别名表条目未命中目标骨架：该关节标为 `unmatched`（`reason=alias_miss`），
  报告列出最近已映射祖先候选，**不做任何静默回退**（名称/路径匹配也不再
  兜底该关节）。
- 多个源关节指向同一目标关节：`E_MAP_CONFLICT` 中止并列出冲突源关节。

## 双路径计算与量化追溯

- 浮点路径全程 float64。
- 定点路径按 `--decimals N` 对局部旋转四元数分量与缩放系数量化，舍入方向
  模式可选 `half_even`（默认）/`half_up`/`down`（向零）/`floor`。
- 每次量化记录 `{frame, joint, stage, component, pre, post, direction,
  rounding}`（绑定对齐阶段 frame=-1），存入对应帧区间的检查点文件，
  使同一帧两路径差异可逐关节逐分量追溯。

## 误差传播模型

对每帧每个已映射目标关节，全局朝向误差上界（弧度）为沿两条链的节点贡献之和：

- 源链（根→源关节）：输入轨道四元数归一化误差 `|1-|q||`；
- 目标链（根→目标关节）：定点量化步长贡献，局部旋转每节点 `2·10^-N`，
  绑定姿态对齐量化每节点 `2·10^-N`（浮点路径为 0）；
- 缩放偏差通道随链记录（对纯朝向贡献为 0，用于归因与清单）。

旋转复合的最坏放大系数每节点取 1.0。上界超过 `--tolerance-deg` 的帧关节
标记为 `untrusted`。归因取贡献最大的节点/通道；最大贡献占比 < 0.5 时判定
无法单一归因，以 `E_ERROR_ATTR_UNKNOWN` 输出候选节点及各自贡献占比。

## 缩放一次性施加

关节 j 的缩放只乘在其自身骨骼（j 的局部平移）上：`scaled = base·s`，
不随父子链重复相乘。每帧每关节输出 `{declared, intended, actual,
apply_diff, declared_diff}` 清单；`|actual - intended| > --scale-tolerance`
时以 `E_SCALE_APPLIED` 定位关节与帧，该路径不再写出姿态结果
（`--inject-scale-dup` 故障注入用于验证该检查）。

## 一致性校验

以源动作全局朝向轨迹经绑定对齐映射后为真值，重定向后对目标骨架重新 FK
计算全局朝向并逐关节逐帧比较角度偏差：输出每关节 max/mean、最大偏差关节、
浮点/定点两路径在同一容差下的判定；超差时定位帧号与关节并归因
（缩放施加错误 > 量化 > 绑定姿态对齐）。报告同时给出浮点/定点两路径的
最大分离（外包方反馈的末端关节漂移症状的可量化复现）。

## 断点恢复

- `ckpt/header.json`：映射表、绑定对齐中间量、运行配置、创世哈希；
- `ckpt/interval_NNNNNN.json`：帧区间姿态、量化事件、误差/缩放记录、
  内容哈希与链式哈希 `h_i = sha256(h_{i-1} + content_hash_i)`；
- `ckpt/manifest.json`：已提交区间数与链头哈希。

所有提交均为临时文件 + 原子重命名。进程被 SIGKILL 后，`resume` 仅依据
磁盘快照逐区间校验快照版本与哈希链，从最后已提交区间之后重算；快照版本
或哈希链不符时以 `E_RECOVERY_INVALID` 定位首个断链帧。恢复过程不写出
退出报告（`report.json`/`trajectory.log`/姿态文件），退出报告仅由
`finalize` 生成。

## 确定性

相同样子与操作序列下，样本、目标姿态与轨迹日志逐字节一致：JSON 序列化
统一 `sort_keys` + 紧凑分隔符，浮点走 `repr` 最短往返格式，日志不含
时间戳，真值来源仅为内容哈希（SHA-256），不使用文件大小或时间戳。

## 运行方式

```bash
uv venv .venv && uv pip install --python .venv/bin/python "numpy>=1.24"
.venv/bin/python -m retarget.cli generate --out artifacts/samples --seed 20261122
.venv/bin/python -m retarget.cli run --anim <anim.json> --target <target.json> --out <run_dir>
.venv/bin/python -m retarget.cli resume --out <run_dir>     # 仅恢复+重算，不写退出报告
.venv/bin/python -m retarget.cli finalize --out <run_dir>   # 生成退出报告
./scripts/acceptance.sh                                     # 一键验收
```

## 实际验证结果（2026-11-22，Python 3.14.4 + NumPy 2.5.5）

`./scripts/acceptance.sh`：**50/50 检查通过**，要点：

- 13 个确定性样本（5/12/18 关节 × 24/120/240 帧 × v1/v2/v3 + 6 个故障样本），
  同种子两次生成逐字节一致；
- 7 个有效样本双路径运行全部 `pass`：浮点路径最大偏差 ≤ 2.5e-6°（数值噪声级），
  定点路径（decimals=4）最大偏差 ≤ 0.0101°，均低于基线摘要预测上界
  （链深 × 2·步长，最大 0.183°）；
- 故障样本按预期返回业务码：`E_ANIM_INVALID`（非单位四元数定位 Spine 帧 7、
  悬空父引用、字段缺失，均无中间产物）、`E_MAP_CONFLICT`（Head/Neck→Cranium）、
  `E_SCALE_APPLIED`（注入重复缩放，定位关节与帧且不写姿态结果）；
- 别名未命中样本非致命完成，映射报告标记 `alias_miss` 并列出最近祖先候选，
  未命中关节保持未映射（无静默回退）；
- 定点 decimals=2 + 容差 0.5° 时不可信标记触发、定点路径判定 `fail`，
  超差归因正确指向 quantization；
- SIGKILL 强制终止后（已提交 2/15 区间）`resume` 仅从区间 2 重算且不写退出
  报告，`finalize` 后与未中断参考运行逐字节一致；
- 篡改区间文件后 `resume` 返回 `E_RECOVERY_INVALID` 并定位首个断链帧（帧 32）；
- 26 个单元测试全部通过。
