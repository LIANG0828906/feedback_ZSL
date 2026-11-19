.PHONY: build test verify init run scenario clean

build:
	go build -o lab/driftd ./cmd/driftd
	go build -o lab/reportmerge ./cmd/reportmerge

test:
	go test ./...

verify:
	scripts/verify.sh

init:
	scripts/init_lab.sh

run: build
	lab/driftd

scenario:
	scripts/scenario.sh

clean:
	rm -rf lab artifacts/scenario artifacts/scenario-report.json

