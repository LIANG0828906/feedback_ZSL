.PHONY: test demo

test:
	python3 -m unittest discover -s tests -v

demo:
	python3 -m a11y_rhythm demo --out artifacts/demo
