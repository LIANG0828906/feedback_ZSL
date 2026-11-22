PYTHON := .venv/bin/python

.PHONY: setup doctor generate test accept paths clean

setup:
	uv python install 3.11
	uv venv --python 3.11 .venv
	uv pip install --python $(PYTHON) -r requirements.txt
	uv pip install --python $(PYTHON) -e .

doctor:
	$(PYTHON) -m mshdr doctor

generate:
	$(PYTHON) -m mshdr generate --seed 20261123 --out samples

test:
	$(PYTHON) -m unittest discover -s tests -v

paths:
	$(PYTHON) -m mshdr run-paths --dataset samples/dataset.json --out outputs/paths --checkpoint-root checkpoints/paths --workers 4

accept:
	$(PYTHON) scripts/run_acceptance.py

clean:
	$(PYTHON) -c "import shutil, pathlib; [shutil.rmtree(p, ignore_errors=True) for p in map(pathlib.Path, ('checkpoints', 'outputs'))]"

