# 快速上手

面向游戏与互动媒体素材的图像取证与完整性桌面工作台。完整设计与实测数据见
[TECHNICAL_NOTES.md](TECHNICAL_NOTES.md)。

## 环境准备

需要 Python 3.9+（开发与验证使用 3.12）。

```bash
python -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt   # PyQt5 / numpy / Pillow
```

## 三步运行

```bash
python main.py generate   # 固定种子生成 6 张样本 + 真值清单到 samples/
python main.py analyze    # 双路径分析，导出报告与三类热力图到 reports/
python main.py gui        # 启动桌面工作台
```

无显示环境：`QT_QPA_PLATFORM=offscreen python main.py gui`。

## 测试

```bash
python -m unittest discover -s tests
```

45 个用例覆盖错误码、哈希/指纹确定性、逐通道分析、双路径一致性、版本快照、
检查点恢复、状态机、异常注入与并发资源边界。

## 模块导航

| 模块 | 职责 |
| --- | --- |
| `forensics/sample_generator.py` | 确定性样本与真值清单 |
| `forensics/loader.py` | 加载、结构校验、分块索引 |
| `forensics/hashing.py` / `analysis.py` / `batch.py` | 哈希指纹与取证统计 |
| `forensics/engine.py` | 预览/精确双路径与 E_DIVERGE 比对 |
| `forensics/params.py` | 参数校验与版本快照 |
| `forensics/checkpoint.py` / `scheduler.py` / `states.py` | 检查点、并发、状态机 |
| `forensics/gui.py` | PyQt5 工作台 |
| `main.py` | CLI 入口 |
