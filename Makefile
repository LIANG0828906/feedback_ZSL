.PHONY: build test race tidy demo clean

build:
	go build ./...

test:
	go test ./... -count=1

race:
	go test -race ./internal/pipeline/ ./internal/store/ -count=1

tidy:
	go mod tidy

# 端到端演示：生成 -> 全量 -> 实时 -> 恢复 -> 三路比对 -> 导出
demo: build
	mkdir -p work
	go run ./cmd/mcapture generate -dir work/sess -channels 4
	go run ./cmd/mcapture run      -dir work/sess -work work/full -workers 3
	go run ./cmd/mcapture realtime -dir work/sess -work work/rt  -workers 1
	go run ./cmd/mcapture resume   -dir work/sess -work work/rs  -workers 3
	go run ./cmd/mcapture verify   -dir work/sess -work work/verify -workers 3
	go run ./cmd/mcapture export   -dir work/sess -work work/full -out work/out

clean:
	rm -rf work
