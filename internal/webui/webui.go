package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:assets
var embedded embed.FS

func FS() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		return embedded
	}
	return sub
}
