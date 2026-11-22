# Windows 10/11 部署

## 前提

- Windows 10/11 x64。
- Python 3.11 x64。
- 本地 wheel 已放入 `wheelhouse/`：
  - `numpy-*-cp311-*-win_amd64.whl`
  - `PySide6-*-cp311-abi3-win_amd64.whl` 或与 Python 3.11 兼容的 PySide6 Windows wheel
  - `shiboken6-*-cp311-abi3-win_amd64.whl`

不访问网络，不依赖 Docker、WSL 或外部素材。

## 一键准备

在仓库根目录打开 PowerShell：

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\setup_windows.ps1 -Python "py -3.11" -Wheelhouse "wheelhouse"
```

脚本执行：

1. 创建 `.venv`；
2. `pip install --no-index --find-links wheelhouse -r requirements.txt`；
3. 生成固定种子样本；
4. 运行 `mediaforge selfcheck`。

## 启动与验收

```powershell
.\.venv\Scripts\python.exe run_workbench.py
.\.venv\Scripts\python.exe -m unittest discover -v
.\.venv\Scripts\python.exe scripts\run_demo.py
```

检查点默认写入 `artifacts\checkpoints`，可在 GUI 的“检查点”页查看、清理和设置最大并行度/缓冲上限。

## 本地 wheel 版本锁定

`requirements.txt` 使用兼容范围；若验收要求固定版本，可改为 wheelhouse 中精确版本，例如：

```text
numpy==2.4.6
PySide6==6.12.0
shiboken6==6.12.0
```

本次算法验证使用 Python 3.11.18、PySide6 6.12.0、numpy 2.4.6；因为交付容器是 WSL2 Linux，Windows 安装脚本未在该主机执行。
