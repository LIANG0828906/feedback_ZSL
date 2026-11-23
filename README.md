# tacpath — 战术寻路与移动决策组件

面向战术决策题材游戏的加权栅格寻路、路径平滑、战术推进推演组件，
全程定点整数计算，支持断点恢复与三路径一致性校验。

## 快速开始

```bash
python3.11 -m venv .venv && .venv/bin/pip install numpy==2.4.6
.venv/bin/python scripts/run_experiment.py all   # 生成样本并运行实验
.venv/bin/python -m unittest discover -s tests   # 运行测试
```

## 模块

- `tacpath/fp.py` 定点数基础（SCALE=1000，代价/坐标/角度）
- `tacpath/mapgen.py` 确定性地图与解析真值生成器
- `tacpath/mapio.py` 地图加载与校验（E_SCHEMA / E_POINT / E_DECODE）
- `tacpath/search.py` 定点化 A*（可配启发式权重、4/8 邻域）
- `tacpath/smooth.py` 视线平滑与拐点提取（定点几何）
- `tacpath/advance.py` 单位战术推进推演（逐 tick 轨迹）
- `tacpath/report.py` 误差传播报告（四项统计 + 累积上界）
- `tacpath/checkpoint.py` 原子串行检查点与版本匹配恢复
- `tacpath/pipeline.py` 三路径编排、比对（E_DIFF）与重放

详见 `docs/REQUIREMENTS.md` 与 `docs/TECHNICAL.md`。
