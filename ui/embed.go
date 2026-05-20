package ui

import (
	"embed"
	"strconv"
	"time"
)

//go:embed static
var StaticFS embed.FS

// AssetVersion is appended as ?v=… in templates to bust browser caches on deploy.
var AssetVersion = strconv.FormatInt(time.Now().Unix(), 36)
