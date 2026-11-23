.PHONY: sample test verify windows-build desktop-build clean

sample:
	go run ./cmd/samplegen -seed 20261124 -out data/scenario.json

test:
	go test ./...

verify:
	go run ./cmd/verify \
		-scenario data/scenario.json \
		-db artifacts/acceptance.db \
		-out artifacts/acceptance-report.json

windows-build:
	cd desktop && mkdir -p build/bin && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o build/bin/tactical-workbench.exe .

desktop-build:
	cd desktop && wails build -platform windows/amd64 -clean

clean:
	rm -r desktop/build artifacts/*.db artifacts/*.db-wal artifacts/*.db-shm 2>/dev/null || true
