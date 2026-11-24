GO ?= go
export CGO_ENABLED ?= 0

.PHONY: all build test verify demo windows-build windows-tests clean

all: test build

build:
	$(GO) build -trimpath -o artifacts/vcomp ./cmd/vcomp

test:
	$(GO) test ./...

samples:
	$(GO) run ./cmd/vcomp gen --out samples --seed 42

verify:
	$(GO) run ./cmd/vcomp verify --data samples --work artifacts/verify

demo:
	$(GO) run ./cmd/vcomp workbench --data samples --version v3 --db artifacts/workbench.db \
		--project workbench --demo --out artifacts/workbench

windows-build:
	mkdir -p artifacts/windows
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -o artifacts/windows/vcomp.exe ./cmd/vcomp

windows-tests:
	mkdir -p artifacts/windows/tests
	@for pkg in $$($(GO) list ./...); do \
		name=$$(printf '%s' "$$pkg" | tr '/' '_'); \
		GOOS=windows GOARCH=amd64 $(GO) test -c -trimpath \
			-o "artifacts/windows/tests/$$name.test.exe" "$$pkg"; \
	done

clean:
	$(GO) clean

