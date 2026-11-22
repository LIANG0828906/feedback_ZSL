# 数字乐谱与演奏表达编辑工作台

面向游戏配乐与互动媒体的多版本乐谱互操作工作台：加载三种 schema 版本的乐谱
交换描述，统一为内部领域模型，编辑音符/力度曲线/表情记号，可逆往返导出，
并基于统一拍位时间轴回放与可视化演奏表达。

## 环境准备（离线、无外部账号）

要求 Python 3.11（仓库已在 Python 3.11.18 验证）。依赖版本固定于
`requirements.txt`：

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt
# 或：python3.11 -m pip install -r requirements.txt
```

## 一键生成确定性样本

```bash
.venv/bin/python -m scripts.generate_samples --output samples
```

固定随机种子 `20261123`，输出 `samples/score_v1.json`、`score_v2.json`、
`score_v3.json` 与 `samples/broken/` 下六类异常样本（损坏、未知版本、缺字段、
悬空事件、语义冲突、重复 ID）。

## 运行 GUI

```bash
.venv/bin/python -m scoreworkbench.app
```

主窗口支持：打开任意版本乐谱、音符/事件表格、提交批次/回滚、选择目标版本导出
（降级时弹窗确认 E_LOSSY）、快照保存/恢复、任意拍位起播/暂停/seek 与三泳道
时间轴可视化。无显示环境时可用 `QT_QPA_PLATFORM=offscreen` 做离屏验证。

## 运行测试

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -t .
```

共 56 个用例，覆盖加载与异常码、升级降级与 E_LOSSY、往返等价与 E_ROUNDTRIP、
编辑一致性与 E_DANGLE/E_PARA、快照原子性与版本作废、回放引擎、离屏 GUI。

## 端到端验收

```bash
.venv/bin/python -m scripts.acceptance_demo \
  --samples samples --report artifacts/acceptance_report.json
```

报告（含每个场景的 PASS/FAIL 与结构化细节）写入
`artifacts/acceptance_report.json`，离屏界面截图见
`artifacts/gui_timeline.png`。

## 目录结构

```
scoreworkbench/
  constants.py errors.py model.py resources.py
  session.py persistence.py playback.py app.py
  versioning/      # v1/v2/v3 加载导出、警告、往返比对
  gui/             # PySide6 主窗口与时间轴
scripts/           # 样本生成、验收驱动
tests/             # 56 个 unittest 用例
docs/ARCHITECTURE.md
```

## 关键设计

详见 `docs/ARCHITECTURE.md`。核心约束：统一拍位时间单位、转换痕迹可追溯、
降级必须显式确认、表达事件强绑定音符、批次原子提交、快照三重版本门控与
原子替换、回放越界返回 `E_PARA`。
