// Package adminapi embeds the hand-written OpenAPI document for the admin API.
package adminapi

import _ "embed"

// Spec is the OpenAPI 3.1 document describing the /api/admin surface, served
// verbatim at GET /api/admin/openapi.json.
//
//go:embed openapi.json
var Spec []byte
