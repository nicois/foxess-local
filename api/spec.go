// Package apispec embeds the API's OpenAPI contract (api/openapi.yaml),
// so the running app serves the exact document it implements.
package apispec

import _ "embed"

// OpenAPI is api/openapi.yaml, verbatim.
//
//go:embed openapi.yaml
var OpenAPI []byte
