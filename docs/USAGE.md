# 使用说明

## 环境准备（Windows 本机等价，本仓库在 WSL/Python 3.11 验证）

```bash
python -m venv .venv
# 已联网缓存时：
.venv/Scripts/python -m pip install -r requirements.txt
# 本机离线环境可用 uv 并复用缓存 wheel：
uv pip install --python .venv/Scripts/python -r requirements.txt
```

## 生成确定性样本

```bash
PYTHONPATH=src python -m subpipeline.cli generate samples --seed 20261123
```

生成 4 个工程：`main`、`narrow` 可完整排布并带真值；`tight` 确定性触发
E_GAP；`tiny` 触发 E_PARA。

## 运行管线

```bash
# 串行
python -m subpipeline.cli run samples/proj_main_20261123 --serial
# 并行（4 线程、4 个缓冲预算）
python -m subpipeline.cli run samples/proj_main_20261123 --workers 4 --buffers 4
# 续跑指定 run 目录
python -m subpipeline.cli run samples/proj_main_20261123 --run-dir runs/run_xxx
```

诊断报告写到 stderr，结构化 JSON 结果写到 stdout。无显示环境时无需任何
桌面组件即可运行；加 `--gui` 才尝试 Tk 面板（失败自动回退无界面）。

## 独立验证

```bash
python -m subpipeline.validator samples/proj_main_20261123 runs/run_xxx --tolerance 0.02
```

退出码：0 通过，1 发现违规，2 样本损坏（E_DECODE）。

## 测试

```bash
PYTHONPATH=src python -m pytest tests/
```
