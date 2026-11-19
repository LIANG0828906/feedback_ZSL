.PHONY: build test scenario direct restart clean

build:
	go build -o bin/coordinator ./cmd/coordinator
	go build -o bin/writer ./cmd/writer
	go build -o bin/loadgen ./cmd/loadgen

test:
	go test ./...

scenario: build
	python3 scripts/run_scenario.py

direct: build
	python3 scripts/run_scenario.py --direct

restart: build
	python3 scripts/run_restart_test.py

clean:
	@echo "Runtime data lives under run/ and can be removed manually."
