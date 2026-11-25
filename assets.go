package main

import "embed"

//go:embed all:frontend/dist
var staticAssets embed.FS
