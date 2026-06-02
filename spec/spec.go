// Package spec embeds the OpenAPI document. openapi.yaml is the source of
// truth; the JSON form served at /openapi.json is derived from it at runtime
// via common/spec, so the two can never drift.
package spec

import (
	_ "embed"

	commonspec "github.com/sweeney/identity/common/spec"
)

//go:embed openapi.yaml
var yamlData []byte

// Converter yields the OpenAPI document as YAML or (lazily converted, cached)
// JSON. Call Converter.JSON() once at startup to fail fast on a malformed spec.
var Converter = commonspec.NewConverter(yamlData)
