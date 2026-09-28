package web

import "embed"

//go:embed static/index.html
var staticIndex []byte

var _ embed.FS
