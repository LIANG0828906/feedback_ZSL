#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
mkdir -p artifacts/acceptance
LOG="artifacts/acceptance/acceptance.log"
exec > >(tee "$LOG") 2>&1

export QT_QPA_PLATFORM=offscreen
PYTHON=".venv/bin/python"

echo "== environment =="
"$PYTHON" --version
"$PYTHON" - <<'PY'
import numpy, PySide6, pytest
print("numpy", numpy.__version__)
print("PySide6", PySide6.__version__)
print("pytest", pytest.__version__)
PY

echo "== deterministic sample generation =="
"$PYTHON" -m score_engine demo --out artifacts/acceptance --seed 20261122

echo "== baseline validation =="
"$PYTHON" -m score_engine validate \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --sequence artifacts/acceptance/baselines/virtual_v3.performance.json \
  > artifacts/acceptance/validation-report.json

echo "== expected version loss =="
set +e
"$PYTHON" -m score_engine convert \
  --score artifacts/acceptance/scores/virtual_v3.json \
  --to 2 \
  > artifacts/acceptance/version-loss-report.json
LOSS_CODE=$?
set -e
if [[ "$LOSS_CODE" -ne 2 ]]; then
  echo "expected E_VERSION_LOSS exit code 2, got $LOSS_CODE"
  exit 1
fi
echo "E_VERSION_LOSS exit code: $LOSS_CODE"

echo "== pytest =="
"$PYTHON" -m pytest -q

echo "== gui smoke =="
"$PYTHON" -m score_engine gui \
  --score artifacts/acceptance/scores/virtual_v3.json --smoke
echo "gui smoke exit code: 0"

echo "== gui screenshots =="
"$PYTHON" - <<'PY'
from PySide6.QtWidgets import QApplication
from score_engine.gui import MainWindow

app = QApplication([])
window = MainWindow("artifacts/acceptance/scores/virtual_v3.json")
window.resize(1200, 820)
window.show()
window.solve_synchronously()
app.processEvents()
for index in range(4):
    window.tabs.setCurrentIndex(index)
    app.processEvents()
    window.grab().save(f"artifacts/acceptance/gui-tab-{index + 1}.png")
window.close()
app.processEvents()
print("wrote 4 GUI tab screenshots")
PY

echo "== artifact digests =="
sha256sum \
  artifacts/acceptance/trace.jsonl \
  artifacts/acceptance/manifests/baseline-manifest.json \
  artifacts/acceptance/manifests/conversion-report.json \
  artifacts/acceptance/gui-tab-1.png \
  artifacts/acceptance/gui-tab-2.png \
  artifacts/acceptance/gui-tab-3.png \
  artifacts/acceptance/gui-tab-4.png

echo "acceptance complete"

