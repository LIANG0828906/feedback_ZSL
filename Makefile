.PHONY: test race vet verify serve clean

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

verify:
	go run ./cmd/ballisticgrid verify --dir artifacts/run

serve:
	go run ./cmd/ballisticgrid serve --addr 127.0.0.1:8088 --dir artifacts/run

clean:
	rm -f artifacts/run/tiles.db artifacts/run/tiles.db.tmp artifacts/run/crash-demo.db
