# ScoreForge — 数字乐谱与演奏表达编辑器

面向游戏配乐与互动媒体制作的专业桌面应用：乐谱工程加载与校验、按小节推进的
编辑会话与提交边界、可暂停/退出/重启续跑的检查点、三路径一致性比对、
基于真值的演奏表达精度校验、时间轴可视化与回放、受控并发提交。

原始任务书见 `docs/TASK.md`，技术设计详见 `docs/TECHNICAL.md`。

## 环境准备

任务书要求 Windows + Python 3.11；实际交付在 Linux (WSL) + Python 3.14 验证，
代码不依赖平台特性，Windows 下步骤相同：

```bash
python -m venv .venv          # 或: uv venv .venv
.venv/Scripts/activate        # Windows；Linux 用 source .venv/bin/activate
pip install -r requirements.txt
```

已验证版本：numpy 2.5.5、PySide6 6.12.0、pytest 9.2.0。
样本与真值由本地确定性生成器产出，无需网络与外部账号。

## 快速开始

```bash
# 生成确定性样本（固定种子 42，3 组工程）
python -m scoreforge.generator --out samples --seed 42 --count 3

# 运行验收套件（加载校验/编辑提交/断点恢复/三路径一致/精度/并发/回放）
python -m scoreforge.acceptance --workdir artifacts/acceptance

# 运行自动化测试（48 项）
python -m pytest tests/ -q

# 启动桌面应用（首次启动自动生成样本；需可用显示会话）
python main.py
```

## 项目结构

```
scoreforge/
  errors.py        统一错误码（E_SCHEMA/E_REF/E_DUR/E_DANGLING/E_BATCH/
                   E_CHECKPOINT/E_DIVERGE/E_PREC），均携带对象路径
  units.py         统一时间单位 tick（TPQ=480）与 TempoMap（tick<->ms）
  generator.py     确定性样本生成器 + 真值参考实现（唯一真值来源）
  validation.py    工程加载与结构校验
  model.py         内部领域模型、依赖索引、事件时间轴、力度曲线（NumPy）
  session.py       编辑会话：工作副本 / 按小节原子提交 / 冻结 / 回滚
  checkpoint.py    检查点：原子串行写入、版本与哈希校验、整体作废
  recompute.py     编辑脚本、三路径（实时/恢复/重算）、E_DIVERGE 比对
  precision.py     基于真值的精度校验与偏差报告（E_PREC）
  concurrency.py   有界线程池并行提交，串行应用，失败隔离与重试
  playback.py      回放引擎：任意拍位起播/暂停，只读不漂移
  gui/             PySide6 桌面界面（时间轴可视化 + 编辑 + 回放）
  acceptance.py    验收运行器，输出 artifacts/acceptance/acceptance_report.txt
tests/             48 项 pytest 测试
main.py            桌面应用入口
samples/           本地生成的确定性样本工程
checkpoints/       本地检查点持久化目录
```

## 实际验证结果

- `python -m pytest tests/ -q`：48 passed
- `python -m scoreforge.acceptance`：7 个验收章节全部通过，失败项 0，
  报告见 `artifacts/acceptance/acceptance_report.txt`
- 异常码演示覆盖：E_SCHEMA / E_REF / E_DUR / E_DANGLING / E_BATCH /
  E_DIVERGE / E_PREC，以及检查点版本与哈希不匹配时的整体作废
