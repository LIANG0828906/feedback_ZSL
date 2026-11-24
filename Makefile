.PHONY: test sample validate full incremental verify export resume serve desktop desktop-check

WORK ?= work
SEED ?= 42
WORKERS ?= 4

test:
	go test ./...
	cd desktop && go test ./...

sample:
	go run ./cmd/vspipeline sample --out "$(WORK)" --seed "$(SEED)"

validate:
	go run ./cmd/vspipeline validate --work "$(WORK)" --workers "$(WORKERS)"

full:
	go run ./cmd/vspipeline full --work "$(WORK)" --workers "$(WORKERS)"

incremental:
	go run ./cmd/vspipeline incremental --work "$(WORK)" --workers "$(WORKERS)" --kind layer_parameter --layer prop --opacity 0.61

verify:
	go run ./cmd/vspipeline verify-incremental --work "$(WORK)" --workers "$(WORKERS)" --layer prop --opacity 0.57

export:
	go run ./cmd/vspipeline export --work "$(WORK)" --version 1
	go run ./cmd/vspipeline export --work "$(WORK)" --version 2
	go run ./cmd/vspipeline export --work "$(WORK)" --version 3

resume:
	go run ./cmd/vspipeline resume --work "$(WORK)"

serve:
	go run ./cmd/vspipeline serve --work "$(WORK)" --workers "$(WORKERS)"

desktop-check:
	cd desktop && go test ./...

desktop:
	cd desktop && wails build -tags webkit2_41
