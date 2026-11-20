@echo off
REM Windows 本地一键环境准备与验收（不使用容器）
setlocal
cd /d %~dp0..

python --version
if errorlevel 1 (
  echo 未找到 Python 3.11+，请先安装并加入 PATH
  exit /b 1
)

python -m venv .venv
call .venv\Scripts\activate.bat
python -m pip install --upgrade pip
python -m pip install -r requirements.txt

python -m replay.cli init-config
python -m replay.cli generate
python -m replay.cli run
python scripts\acceptance.py

echo 验收完成，启动图形界面...
python -m replay
endlocal
