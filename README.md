# 哥特玫瑰窗离线软光栅验证组件

面向叙事游戏关卡美术的确定性离线渲染验证工具：从一份 JSON 场景出发，逐像素核对
彩绘玻璃的多波段透射、铅条墙体投影落点、软阴影边缘与釉面反射/透射占比，并通过
分块快照保证中断续算与一次性渲染逐字节一致。纯本地运行，不依赖外部模型、贴图或网络。

## 环境准备

- Windows + Python 3.11 或 3.12（当前开发机以 Python 3.12.16 验证）。
- 依赖固定为 NumPy 1.26.4、Pillow 10.2.0，均提供 Windows wheels。

```bat
scripts\setup_windows.bat
```

Linux/macOS 等价命令：

```bash
python3.12 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

## 命令

```bash
export PYTHONPATH=src

# 1. 生成确定性场景（--invalid 可生成六类损坏样本）
python -m rosewin.cli generate-scene --output build/scene.json

# 2. 完整渲染（存在有效快照时自动续算）
python -m rosewin.cli render --scene build/scene.json --output-dir build/render

# 3. 可控中断：完成 2 块后提交快照并以退出码 0 结束
python -m rosewin.cli render --scene build/scene.json --output-dir build/render --interrupt-after 2

# 4. 基线-中断-续算-逐字节比对
python -m rosewin.cli verify-recovery --scene build/scene.json --workspace build/recovery_check

# 5. 场景校验（失败写结构化错误 JSON，退出码 2，不产生图像）
python -m rosewin.cli validate --scene build/bad.json --error-report build/error.json
```

一键验收（Windows）：`scripts\acceptance_windows.bat`。

## 输出

- `render.png`：`sqrt` gamma 合成图；`transmission.png`、`reflection.png`：分离的两层；
  `lead_mask.png`：铅条软边覆盖；`shadow.png`：墙体软阴影。
- `owner.png`：逐像素玻璃片归属（0 为背景，1..20 为稳定片 ID）；
  `owner_lanes.npz`：每像素全部命中片，按 `stable_id` 升序，与输入顺序无关。
- `render.snapshot.npz`：分块快照，绑定场景 SHA-256，原子整体替换。
- `run_report.json`：象限可见片元、逐片反射/透射占比、投影采样点与落点、
  软阴影边缘宽度、分块摘要、图像摘要与恢复次数；字段顺序稳定，无时间戳与绝对路径。
- `verification_report.json` / `recovery_diff.json`：恢复一致性与结构化差异。

## 测试

```bash
PYTHONPATH=src python -m unittest discover -s tests
```

19 个测试覆盖校验业务码、纹理逐字节确定性、分层不溢出、红蓝可辨、软阴影深度趋势、
快照损坏/摘要不符丢弃与恢复逐字节一致。设计与公式见 `docs/TECHNICAL_NOTES.md`。
