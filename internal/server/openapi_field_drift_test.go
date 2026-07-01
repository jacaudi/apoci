package server

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/admin"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/replication"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server/adminapi"
)

// specSchemas is the slice of the OpenAPI document this test cares about: the
// declared property set of each named component schema.
type specSchemas struct {
	Components struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
}

// jsonFieldNames returns the set of JSON property names encoding/json would emit
// for struct type t, derived from the type (not a marshalled value) so it
// reflects the *declared* shape rather than what survives omitempty at runtime:
//   - the json tag name wins; an untagged exported field falls back to its Go name
//   - json:"-" drops the field
//   - omitempty/omitzero are ignored (they gate runtime presence, not the schema)
//   - an anonymous embedded struct with no json name has its fields promoted
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	names := make(map[string]struct{})
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			maps.Copy(names, jsonFieldNames(f.Type))
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names[name] = struct{}{}
	}
	return names
}

// TestAdminOpenAPISchemasMatchStructs guards field-level drift between the
// hand-written admin OpenAPI spec and the Go structs that back its response and
// request schemas. For each struct-backed schema it asserts the spec's declared
// properties exactly match the struct's JSON fields — failing on a missing OR an
// extra property. Ad-hoc map responses (including the ReplicationStatus wrapper,
// which has no Go struct) are deliberately NOT covered here; see
// docs/findings/openapi-field-drift.md.
func TestAdminOpenAPISchemasMatchStructs(t *testing.T) {
	var spec specSchemas
	require.NoError(t, json.Unmarshal(adminapi.Spec, &spec), "embedded spec must be valid JSON")

	cases := []struct {
		schema string
		typ    reflect.Type
	}{
		{"GCStatus", reflect.TypeFor[peering.GCStatus]()},
		{"ImageEntry", reflect.TypeFor[admin.ImageEntry]()},
		{"Actor", reflect.TypeFor[database.Actor]()},
		{"TargetStatus", reflect.TypeFor[replication.TargetStatus]()},
		{"FollowRequest", reflect.TypeFor[adminFollowRequest]()},
		{"FollowFilterRequest", reflect.TypeFor[adminFollowFilterRequest]()},
		{"PeerBlockRequest", reflect.TypeFor[adminPeerBlockRequest]()},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			schema, ok := spec.Components.Schemas[tc.schema]
			require.Truef(t, ok, "schema %q missing from spec components.schemas", tc.schema)

			want := jsonFieldNames(tc.typ)
			got := make(map[string]struct{}, len(schema.Properties))
			for name := range schema.Properties {
				got[name] = struct{}{}
			}

			for name := range want {
				if _, present := got[name]; !present {
					t.Errorf("schema %q is missing property %q declared by %s", tc.schema, name, tc.typ)
				}
			}
			for name := range got {
				if _, present := want[name]; !present {
					t.Errorf("schema %q declares property %q not present on %s", tc.schema, name, tc.typ)
				}
			}
		})
	}
}
