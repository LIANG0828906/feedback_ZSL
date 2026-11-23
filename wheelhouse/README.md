# Local wheelhouse

离线安装时把与 Windows + Python 3.11 x64 匹配的 wheel 放在本目录：

- `numpy-*-cp311-*-win_amd64.whl`
- `PySide6-*-cp311-abi3-win_amd64.whl`
- `shiboken6-*-cp311-abi3-win_amd64.whl`

安装入口：

```powershell
.\scripts\setup_windows.ps1 -Python "py -3.11" -Wheelhouse "wheelhouse"
```

Linux/WSL 离线入口（优先复用本地已安装的离线解释器，存在 venv 工具时才从本目录装 wheel）：

```bash
./scripts/setup_linux.sh
```

核心算法不依赖 wheel；缺少 GUI wheel 时，CLI、样本生成、封装、校验和全部无 GUI 测试仍可运行。当前工作区的本地解释器 `/usr/bin/python3` 已预装 numpy 与 PySide6，可直接运行：

```bash
python3 -m unittest discover -v
python3 -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
```
