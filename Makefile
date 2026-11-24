.PHONY: test verify scene ui windows

test:
	CGO_ENABLED=0 go test ./...

verify:
	CGO_ENABLED=0 bash scripts/verify.sh

scene:
	CGO_ENABLED=0 go run ./cmd/scene-gen -seed 1042 -count 3 -out data/projects

ui:
	CGO_ENABLED=0 go run . -addr 127.0.0.1:34115

windows:
	CGO_ENABLED=0 wails build -tags wails -skipbindings -s -platform windows/amd64 -nopackage -clean -o OcclusionLab.exe
