# Battle Balance Lab · 战斗数值平衡验证工作台

面向本地多人对战题材游戏的确定性战斗数值平衡工作台。项目从空白工作区构建，包含固定种子样本生成器、定点战斗结算、受限资源并行调度、预算取舍、双层状态机、SQLite 检查点、三路径一致性比对、平衡报告和 PySide6 桌面诊断界面。

![诊断界面](artifacts/ui_overview.png)

## 快速开始（Windows PowerShell）

目标环境为原生 Windows、Python 3.11，不使用 Docker、GPU 或在线服务。

```powershell
cd <项目目录>
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\setup_windows.ps1
.\.venv\Scripts\python.exe run_app.py
```

测试与验收：

```powershell
$env:QT_QPA_PLATFORM = "offscreen"
.\.venv\Scripts\python.exe -m pytest -q
.\.venv\Scripts\python.exe scripts\run_demo.py
```

Linux/WSL 等价命令：

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python numpy PySide6 pytest
PYTHONPATH=. .venv/bin/python scripts/generate_samples.py
QT_QPA_PLATFORM=offscreen PYTHONPATH=. .venv/bin/python -m pytest -q
PYTHONPATH=. .venv/bin/python scripts/run_demo.py
PYTHONPATH=. .venv/bin/python run_app.py
```

本次实际验收使用 `Python 3.11.18 + PySide6 6.12.0 + NumPy 2.4.6`，运行于 WSL2/Ubuntu 26.04；应用与测试代码仅使用跨平台 Python/Qt 接口，Windows 脚本已提供。运行期间不访问外部服务。

## 已实现能力

- **工程加载与结构校验**：严格检查字段、定点整型、技能/公式/配装引用、公式 DAG 无环、场景与主配装一一对应；错误精确定位到对象与字段。
- **定点战斗核心**：公式区间使用整数求值，伤害、生命、资源全部为整数累加；冷却、资源、命中波动与小队多单位结算完整。随机源仅由比赛种子、回合和阵营标签哈希派生，不使用全局 RNG。
- **受限并行调度**：候选 × 场景组合在固定槽位中分批推进，超过槽位/每轮上限的组合阻塞排队，不丢弃；结果和提交顺序与串行路径完全一致。
- **预算取舍**：按“最小覆盖 → 按候选优先级公平扩样 → 资源封顶”的可解释规则分配采样，记录每个组合的内存/时间上限、实际场次、置信区间和削减原因。
- **双层状态机**：方案与组合分别执行 `PENDING → EVALUATING/SIMULATING → SUBMITTED/FAILED`；非法迁移返回 `E_STATE`，已提交组合冻结。
- **原子检查点**：每次提交组合/方案都以 SQLite 事务串行写入完整版本信封、结果哈希与提交顺序；任一工程哈希、候选集哈希、预算版本或算法版本不匹配时整体删除快照并全量重算。
- **三路径一致与自动定位**：实时、断点恢复、一次性全量三条路径逐场比对；首个不一致会定位到候选 ID、场景 ID、采样序号和回合序号并返回 `E_DIFF`。
- **专业诊断界面**：职业/技能/公式依赖图、候选状态、胜率置信区间、逐回合伤害、预算水位、取舍序列表、阈值命中、报告导出和系数增量探针。

## 目录

```text
balance_lab/
  generator.py      固定种子工程、候选、解析真值、异常注入清单
  validation.py     严格结构校验与内部模型加载
  combat.py         定点公式 DAG 求值、战斗结算、逐回合结果
  budget.py         采样预算规划与可重放取舍决策
  scheduler.py      并行槽位、排队、状态推进、检查点提交
  state.py          方案/组合双层状态机与冻结结果
  checkpoint.py     SQLite 原子检查点与整体版本校验
  compare.py        多路径逐场比对与 E_DIFF 定位
  report.py         JSON/Markdown 平衡验证报告
  ui/               PySide6 桌面应用
data/samples/       3 套确定性数值工程、候选方案、注入清单
tests/              30 项自动化测试
scripts/            Windows 安装、样本生成、一键验收
artifacts/          实际验收记录、三套报告与界面截图
docs/               设计与验收说明
```

## 样本规模

每套工程包含 3 个职业、9 个技能、30 个公式节点、6 套配装、6 个场景（4 单挑 + 2 小队）和 7 个候选方案。三套工程分别声明 3/2/4 个模拟槽位、每轮 5/3/7 个组合上限，以及不同的内存与单场景时长预算。

固定种子为 `420143 / 730021 / 991337`，manifest 哈希记录在 `data/samples/manifest.json`。重新生成相同种子会得到逐字节一致的数据文件。

## 定点尺度

- 伤害/生命：milli-HP 整数，`1000 = 1 HP`
- 成长系数/缩放因子/命中波动：basis points，`10000 = 1.0`
- 资源：milli-resource 整数
- 公式运算：`ADD / SUB / MAX / MIN / MUL_BP / DIV_BP`，除法向下取整且分母不小于 1

战斗校准不使用浮点；浮点仅用于 Wilson 置信区间等统计展示。

## 错误码

| 错误码 | 语义 | 典型定位 |
|---|---|---|
| `E_SCHEMA` | 缺字段、类型非法、定点字段为浮点/布尔、场景配装映射非法 | `skill.iron.strike.formula_id` |
| `E_CYCLE` | 公式依赖图成环 | `formula.injected.cycle_a.args` |
| `E_REF` | 技能、公式、职业或配装引用不存在 | `class.iron.skills` |
| `E_PARA` | 预算、采样或候选调整参数越界 | `request.target_samples` |
| `E_STATE` | 非法状态迁移或修改已冻结组合 | `candidate::scenario.state` |
| `E_DIFF` | 三路径逐场结果不一致 | 候选/场景/采样/回合 |

项目错误注入清单位于 `data/samples/injection_cases.json`，运行时异常链路由测试覆盖。

## 预算策略

1. 每个组合先获得可承受的最小采样。
2. 每线程缓冲上限 = `memory_buffer_kb / parallel_slots`；内存结果估算与单场景时长共同形成组合硬上限。
3. 在硬上限内按候选优先级和稳定组合顺序逐轮扩样。
4. 全局采样预算耗尽或资源封顶时记录 `REDUCE_MATCHES`，组合仍保留在队列中。
5. 槽位不足只影响排队和调度轮次，不影响最终组合覆盖。

同一输入下预算决策序列完全可重放；`decision_hash` 会写入验收记录。

## 持久化与强制终止边界

检查点文件不承诺代表“内存中的完整运行”。强制终止进程时，只能恢复最后一条已原子提交的组合/方案记录；应用不会虚构缺失组合，也不会混用旧版本快照。重启后若版本信封任一字段不匹配，整个快照被删除并全量重算。

## 实际验收

详见 [artifacts/verification.md](artifacts/verification.md) 与 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

核心结果：

- `30 passed`
- 三路径逐场一致：PASS
- 串行 vs 并行结果与提交顺序一致：PASS
- 断点恢复复用 15 个组合、补算 27 个组合
- 7/7 工程错误注入命中预期错误码、对象和字段
- 检查点版本不匹配：49 行快照整体作废为 0 行
- 三套工程各 42 个组合全部提交，预算不足时均有可解释削减记录

## 设计与边界说明

- [docs/DESIGN.md](docs/DESIGN.md)：数据模型、算法、调度、状态机、检查点和界面结构。
- [docs/VERIFICATION.md](docs/VERIFICATION.md)：本次实际运行记录、测试覆盖和复现步骤。
- UI 中的“系数探针”是独立增量评估：主验证已提交组合保持冻结，探针不会覆盖主结果。
- 解析真值中，公式伤害区间是整数公式精确推导；胜率区间是 256 种子基线校准带，并留出确定性采样前缀容差。
