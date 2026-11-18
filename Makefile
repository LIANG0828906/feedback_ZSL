GO ?= go
GOMAXPROCS ?= 2

.PHONY: build test vet run run-redis check

build:
	GOMAXPROCS=$(GOMAXPROCS) $(GO) build -o bin/archive-server ./cmd/archive-server
	GOMAXPROCS=$(GOMAXPROCS) $(GO) build -o bin/archive-check ./cmd/archive-check

test:
	GOMAXPROCS=$(GOMAXPROCS) $(GO) test ./...

vet:
	GOMAXPROCS=$(GOMAXPROCS) $(GO) vet ./...

run:
	./bin/archive-server -config config/acceptance-final.json

run-redis:
	mkdir -p var/redis
	.tools/redis-7.2.7/src/redis-server --bind 127.0.0.1 --port 6379 --daemonize yes \
		--pidfile "$(CURDIR)/var/redis/redis.pid" \
		--logfile "$(CURDIR)/var/redis/redis.log" \
		--dir "$(CURDIR)/var/redis" --save '' --appendonly no \
		--maxmemory 256mb --maxmemory-policy noeviction

check:
	./bin/archive-check -addr http://127.0.0.1:18086 -mode healthy -ranges 2
