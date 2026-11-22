# 安装与运行

## 环境要求

- Windows 10/11 或 Linux/WSL2。
- Python 3.10 及以上，必须支持 `venv`。
- 本地可写工作区，用于 `data/`、`var/checkpoints/` 和 `artifacts/`。
- 不使用 Docker、虚拟机或其他容器环境。

本工作区实际验证环境：

```text
Python 3.14.4
NumPy 2.5.5
PySide6 6.12.0
WSL2 Ubuntu 26.04 / Linux 6.18
```

Windows 上使用本地 Python 即可；上述 WSL2 结果只代表本次实际验证环境。

## Windows 安装

```powershell
cd <工作区目录>
py -3.12 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -e .
```

如果 PowerShell 禁止执行激活脚本，可直接使用虚拟环境中的 Python：

```powershell
.\.venv\Scripts\python.exe -m pip install --upgrade pip
.\.venv\Scripts\python.exe -m pip install -e .
.\.venv\Scripts\python.exe -m vector_workbench generate --out data
.\.venv\Scripts\python.exe -m vector_workbench gui --data data
```

`pip` 会自动选择当前 Python 可用的 NumPy 与 PySide6 版本。项目没有锁定补丁版本；验收记录保留本次实际版本。

## Linux / WSL 安装

Debian/Ubuntu 若提示 `ensurepip is not available`，先安装系统 venv 组件：

```bash
sudo apt update
sudo apt install python3-venv
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e .
```

本次环境没有可用的 `ensurepip` 且无交互式 `sudo`，因此实际使用：

```bash
python3 -m venv --without-pip .venv
curl -fsSL https://bootstrap.pypa.io/get-pip.py -o /tmp/get-pip.py
.venv/bin/python /tmp/get-pip.py
.venv/bin/python -m pip install numpy PySide6
```

该做法仅为本机环境记录，Windows 上优先使用标准 `venv`。

## 准备数据与持久化目录

```bash
python -m vector_workbench generate --out data --seed 20261123
mkdir -p var/checkpoints
test -w var/checkpoints && echo writable
```

生成器会创建：

- `data/scenes/*.json`
- `data/truth/truth_manifest.json`
- `data/samples.json`

验收命令也会创建 `artifacts/`。若需要重新生成全部样本，重复执行 `generate` 即可；固定种子下输出逐字节一致。

## 启动界面

```bash
python -m vector_workbench gui --data data --checkpoints var/checkpoints/gui
```

指定场景：

```bash
python -m vector_workbench gui --data data \
  --checkpoints var/checkpoints/gui --scene-id badge_combo
```

无显示环境：

```bash
QT_QPA_PLATFORM=offscreen python -m vector_workbench gui --offscreen
```

## 版本确认

```bash
python - <<'PY'
import sys, numpy, PySide6
print(sys.version)
print("NumPy", numpy.__version__)
print("PySide6", PySide6.__version__)
PY
```

