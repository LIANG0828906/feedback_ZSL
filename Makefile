.PHONY: test verify ui ui-ci windows clean

test:
	go test ./...

verify:
	go run ./cmd/glyphbrush verify -samples ./samples -out ./artifacts/verification -checkpoints ./artifacts/verification/checkpoints

ui:
	go run -tags fyne ./cmd/glyphbrush-ui -samples ./samples

ui-ci:
	go build -tags 'fyne ci' ./cmd/glyphbrush-ui

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o artifacts/bin/glyphbrush.exe ./cmd/glyphbrush

clean:
	go clean
