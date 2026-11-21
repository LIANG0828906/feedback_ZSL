# 本地素材审看系统实现说明

完整设计见 `docs/DESIGN.md`，源码位于 `mediaaudit/`，测试位于 `tests/`。

## 快速开始

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# 无显示环境跑完整确定性流程
python -m mediaaudit.cli --work-dir ./work --sample stereo_48k

# Windows 桌面会话启动诊断界面
python -m mediaaudit.gui

# 全量测试
QT_QPA_PLATFORM=offscreen pytest -q
```

## 模块映射

| 业务要求 | 模块 |
| --- | --- |
| 1 源读取与整数时基 | `mediaaudit/timebase.py` |
| 2 参数版本模型 | `mediaaudit/parameters.py` |
| 3 切片生成与内容哈希 | `mediaaudit/slicing.py` |
| 4 依赖图谱与增量重算 | `mediaaudit/graph.py`、`mediaaudit/engine.py` |
| 5 回退失效与陈旧拒绝 | `mediaaudit/engine.py` |
| 6 跨档起止样本校验 | `mediaaudit/span.py` |
| 7 原子提交与恢复 | `mediaaudit/storage.py` |
| 8 诊断界面与样本入口 | `mediaaudit/gui.py`、`mediaaudit/samplegen.py`、`mediaaudit/cli.py` |

## 产物目录

- `work/sources/`：确定性 WAV 与 JSON 帧描述、`baseline.json`
- `work/audit/slices/`：切片内容文件（原子写入）
- `work/audit/manifest.jsonl`：哈希链索引清单
- `work/audit/parameters.json`：参数版本快照
- `work/audit/trajectory.jsonl`：版本号、切片序号、失效原因、复用/重算片数与内容哈希轨迹
