PYTHON ?= .venv/bin/python

.PHONY: test demo

test:
	$(PYTHON) -m pytest -q

demo:
	$(PYTHON) -m festival_master demo --workspace deliverable --duration 7.0 --block-samples 4096
