@echo off
setlocal
cd /d "%~dp0.."
if not exist ".venv\Scripts\python.exe" (
  echo Run scripts\setup.bat first.
  exit /b 1
)
set PY=.venv\Scripts\python.exe
if not exist reports mkdir reports
%PY% scripts\run_scenario.py --tenant tenant-a --protocol-version v3 --count 5000 --rate 0 --canary-weight 0.0 --timeout-seconds 15 --output reports\workload_v3_5000.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 5000 --rate 0 --canary-weight 1.0 --timeout-seconds 15 --output reports\workload_v4_5000.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-d --protocol-version mixed --count 120 --rate 200 --canary-weight 0.5 --timeout-seconds 15 --output reports\scenario_mixed.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 20 --rate 200 --canary-weight 1.0 --misalign-rate 1.0 --timeout-seconds 15 --output reports\scenario_misalign.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-c --protocol-version v3 --count 50 --rate 200 --canary-weight 1.0 --inject-load-failure --timeout-seconds 15 --output reports\scenario_load_failure.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 80 --rate 100 --canary-weight 1.0 --delay-rate 1.0 --delay-ms 150 --timeout-seconds 15 --output reports\scenario_rollback.json
if errorlevel 1 exit /b 1
%PY% scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 45 --rate 100 --canary-weight 1.0 --delay-rate 1.0 --delay-ms 700 --timeout-seconds 15 --output reports\scenario_timeout.json
if errorlevel 1 exit /b 1
echo Scenario reports written to reports\.
