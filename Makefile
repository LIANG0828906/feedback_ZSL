PYTHON ?= .venv/bin/python

.PHONY: setup test demo determinism gui-smoke

setup:
	uv venv --python 3.14 .venv
	uv pip install --python $(PYTHON) -r requirements.txt

test:
	$(PYTHON) -m unittest discover -s tests -v

demo:
	$(PYTHON) -m fpstore.cli demo --work artifacts/demo --seed 20261121 --crash-after 3

determinism:
	$(PYTHON) -m fpstore.cli demo --work artifacts/determinism-check --seed 20261121 --crash-after 3 --verify-determinism

gui-smoke:
	QT_QPA_PLATFORM=offscreen $(PYTHON) -m fpstore.gui --work artifacts/gui-smoke --offscreen-smoke
