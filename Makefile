.PHONY: generate validate test verify bake ui

CGO_ENABLED ?= 0

generate:
	CGO_ENABLED=$(CGO_ENABLED) go run ./cmd/terrainctl generate -out samples/terrain_project.json

validate:
	CGO_ENABLED=$(CGO_ENABLED) go run ./cmd/terrainctl validate -project samples/terrain_project.json

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./... -count=1

verify:
	CGO_ENABLED=$(CGO_ENABLED) go run ./cmd/terrainctl verify -project samples/terrain_project.json -dir artifacts/verify -out artifacts/verification.json

bake:
	CGO_ENABLED=$(CGO_ENABLED) go run ./cmd/terrainctl bake -project samples/terrain_project.json -db artifacts/terrain.sqlite -full -report artifacts/bake_report.json -export artifacts/export

ui:
	CGO_ENABLED=0 wails build -platform windows/amd64 -nosyncgomod -s -skipbindings -nopackage
