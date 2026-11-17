# 离线空间声景渲染组件

从零构建的确定性离线渲染链路：多层几何遮挡透射、动态混响过渡、解析双耳声像，
输出立体声母带与逐帧空间音频报告，支持分块快照与强制终止恢复。

## 快速开始

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python "numpy>=1.24" "scipy>=1.10"
.venv/bin/python -m space_audio.cli generate --scene demo/scene.json
.venv/bin/python -m space_audio.cli render --scene demo/scene.json --work-dir demo/out_full
.venv/bin/python -m unittest discover -s tests
```

详见 `docs/TECHNICAL_NOTES.md`。
