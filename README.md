# 陶窑玻璃砖墙确定性渲染器（feedback_ZSL）

纯 Python 3.11 + NumPy 的 CPU 确定性渲染器，模拟陶窑观察窗三层半透明玻璃砖的
透射、应力条纹散射、多火口砖缝软阴影投影，并支持分块快照与强制终止后续算。

## 快速开始

```bash
uv venv --python 3.11 .venv && . .venv/bin/activate
uv pip install -r requirements.txt
python -m kiln_glass generate --scene artifacts/scene.json
python -m kiln_glass validate --scene artifacts/scene.json
python -m kiln_glass render  --scene artifacts/scene.json --output artifacts/run_full
python -m unittest discover -s tests
```

## 目录

- `kiln_glass/`：场景、校验、几何、光照、渲染、快照、CLI 源码
- `tests/`：36 个 unittest 用例（校验、确定性、渲染、快照续算、CLI）
- `docs/TECHNICAL_DESIGN.md`：架构与 8 项业务要求对照
- `docs/VERIFICATION.md`：实测数据、退出码与复现步骤
- `artifacts/`：确定性场景 JSON 与示例渲染产物
