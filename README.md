# 游戏过场字幕无障碍排版管线

从零实现的字幕时间轴 + 版面双维排布管线、确定性样本生成器、并行调度、
检查点续跑与独立结果验证器。纯 Python + numpy，无容器依赖。

## 环境

- Python 3.10+（仓库在 Python 3.11.18 验证，Windows 本机同版本可用）。
- 依赖见 `requirements.txt`：`numpy 2.2.6`、`pytest 8.4.2`（已进入本机
  uv 缓存，可离线复装）。

```bash
python -m venv .venv
uv pip install --python .venv/bin/python -r requirements.txt   # 或 pip install -r
```

## 目录

- `src/subpipeline/`：管线全部模块（见 `docs/DESIGN.md`）。
- `tests/`：41 项 pytest，覆盖 8 个错误码与三条路径一致性。
- `scripts/three_path_demo.py`：串行/并行/续跑三路径一致性演示。
- `samples/`：固定种子（20261123）生成的 4 个确定性工程。
- `runs/`：每个 run 独立、带唯一 ID 的检查点目录，不与其他任务共用。
- `docs/`：设计与使用文档。

## 快速开始

```bash
# 1. 生成确定性样本与解析真值
PYTHONPATH=src python -m subpipeline.cli generate samples
# 2. 串行 / 并行排布
PYTHONPATH=src python -m subpipeline.cli run samples/proj_main_20261123 --serial
PYTHONPATH=src python -m subpipeline.cli run samples/proj_main_20261123 --workers 4
# 3. 独立验证（仅读持久化结果 + 清单）
PYTHONPATH=src python -m subpipeline.validator samples/proj_main_20261123 <run_dir>
# 4. 全量测试
PYTHONPATH=src python -m pytest tests/
```

## 确定性样本工程

| 工程 | 用途 | 预期结果 |
| --- | --- | --- |
| `proj_main_*` | 常规行宽、含显式窗口 | 全部提交，带解析真值 |
| `proj_narrow_*` | 6 字窄行宽、多屏切分 | 全部提交，带解析真值 |
| `proj_tight_*` | 零空隙背靠背语音段 | 确定性 `E_GAP`，失败隔离 |
| `proj_tiny_*` | 行宽 0 + 长英文词 | 确定性 `E_PARA` |

## 错误码

| 码 | 触发 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、类型/正值校验失败 |
| `E_REF` | 台词引用不存在的语音段 |
| `E_WIN` | 显式窗口越出所属语音段 |
| `E_GAP` | 无可延展空隙仍不满足可读时长 |
| `E_PARA` | 行宽无法容纳单原子/无合法折行 |
| `E_TIMING` | 穷尽取舍后窗口仍短于所需可读时长 |
| `E_STATE` | 台词状态机非法迁移 |
| `E_DECODE` | 清单/波形/检查点记录损坏 |

所有错误均带台词编号（适用时）与字段名。

## 实际验证结果

- `pytest`：41 passed。
- 四个工程的包络检测边界与声明语音段逐段精确一致。
- 主工程串行、4 线程并行、检查点续跑三条路径的显示窗口、逐行折行、
  提交序列完全一致（见 `scripts/three_path_demo.py`）。
- 独立验证器对正常 run 返回 ok；篡改一条记录后正确报出首个违规台词编号、
  违规项与实测值；写入损坏记录返回 `E_DECODE`。
- 无界面模式独立可运行；Tk 诊断面板为可选项，无显示环境自动回退。
