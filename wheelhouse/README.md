# Local wheelhouse

离线安装时把与 Windows + Python 3.11 x64 匹配的 wheel 放在本目录：

- `numpy-*-cp311-*-win_amd64.whl`
- `PySide6-*-cp311-abi3-win_amd64.whl`
- `shiboken6-*-cp311-abi3-win_amd64.whl`

安装入口：

```powershell
.\scripts\setup_windows.ps1 -Python "py -3.11" -Wheelhouse "wheelhouse"
```

核心算法不依赖 wheel；缺少 GUI wheel 时，CLI、样本生成、封装、校验和全部无 GUI 测试仍可运行。
