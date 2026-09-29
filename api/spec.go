// Package api embeds the generated public REST contract for thin adapters.
package api

import _ "embed"

// OpenAPI is generated from openapi.yaml and the authoritative shared schemas.
//
//go:embed gen/openapi.bundle.json
var OpenAPI []byte
