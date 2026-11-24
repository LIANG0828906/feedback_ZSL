.PHONY: test race sample verify verify-offline wails-build clean-generated

test:
	go test -count=1 ./...

race:
	go test -count=1 -race ./internal/...

sample:
	go run ./cmd/samplegen -out artifacts/sample

verify:
	go run ./cmd/volumediag verify -scene artifacts/sample -db artifacts/verification.db -report artifacts/verification.json

verify-offline:
	GOPROXY=off go test ./...

wails-build:
	wails build -tags desktop

clean-generated:
	rm -f artifacts/verification.db artifacts/verification.db-wal artifacts/verification.db-shm
	rm -f artifacts/verification.db.resume artifacts/verification.db.resume-wal artifacts/verification.db.resume-shm
