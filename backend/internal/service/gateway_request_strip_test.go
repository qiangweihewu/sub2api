package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStripOpenAIToolSchemaArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantChange bool
		check      func(t *testing.T, result []byte)
	}{
		{
			name:       "no tools field",
			body:       `{"model":"claude-3","messages":[]}`,
			wantChange: false,
		},
		{
			name:       "empty tools array",
			body:       `{"model":"claude-3","tools":[]}`,
			wantChange: false,
		},
		{
			name: "tool without artifacts — unchanged",
			body: `{"model":"claude-3","tools":[{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`,
			wantChange: false,
		},
		{
			name: "strips additionalProperties from input_schema",
			body: `{"model":"claude-3","tools":[{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}}]}`,
			wantChange: true,
			check: func(t *testing.T, result []byte) {
				require.False(t, gjson.GetBytes(result, "tools.0.input_schema.additionalProperties").Exists())
				require.Equal(t, "object", gjson.GetBytes(result, "tools.0.input_schema.type").String())
			},
		},
		{
			name: "strips $schema from input_schema",
			body: `{"model":"claude-3","tools":[{"name":"read","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{}}}]}`,
			wantChange: true,
			check: func(t *testing.T, result []byte) {
				schema := gjson.GetBytes(result, "tools.0.input_schema")
				require.True(t, schema.Exists())
				// $schema should be gone
				schema.ForEach(func(key, _ gjson.Result) bool {
					require.NotEqual(t, "$schema", key.String())
					return true
				})
			},
		},
		{
			name: "strips nested additionalProperties in properties",
			body: `{"model":"claude-3","tools":[{"name":"t","input_schema":{"type":"object","properties":{"nested":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}}}}]}`,
			wantChange: true,
			check: func(t *testing.T, result []byte) {
				require.False(t, gjson.GetBytes(result, "tools.0.input_schema.properties.nested.additionalProperties").Exists())
				require.Equal(t, "string", gjson.GetBytes(result, "tools.0.input_schema.properties.nested.properties.x.type").String())
			},
		},
		{
			name: "strips from items (array schema)",
			body: `{"model":"claude-3","tools":[{"name":"t","input_schema":{"type":"array","items":{"type":"object","additionalProperties":false}}}]}`,
			wantChange: true,
			check: func(t *testing.T, result []byte) {
				require.False(t, gjson.GetBytes(result, "tools.0.input_schema.items.additionalProperties").Exists())
				require.Equal(t, "object", gjson.GetBytes(result, "tools.0.input_schema.items.type").String())
			},
		},
		{
			name: "multiple tools — only affected ones changed",
			body: `{"model":"claude-3","tools":[
				{"name":"clean_tool","input_schema":{"type":"object","properties":{}}},
				{"name":"dirty_tool","input_schema":{"type":"object","additionalProperties":false,"properties":{"q":{"type":"string"}}}}
			]}`,
			wantChange: true,
			check: func(t *testing.T, result []byte) {
				require.False(t, gjson.GetBytes(result, "tools.1.input_schema.additionalProperties").Exists())
				require.Equal(t, "clean_tool", gjson.GetBytes(result, "tools.0.name").String())
				require.Equal(t, "dirty_tool", gjson.GetBytes(result, "tools.1.name").String())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, changed := stripOpenAIToolSchemaArtifacts([]byte(tt.body))
			require.Equal(t, tt.wantChange, changed)
			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

func TestStripThirdPartyBodyFields_IncludesToolSchemaStripping(t *testing.T) {
	body := `{"model":"claude-3","service_tier":"default","tools":[{"name":"t","input_schema":{"type":"object","additionalProperties":false}}]}`
	result, changed := stripThirdPartyBodyFields([]byte(body))
	require.True(t, changed)
	require.False(t, gjson.GetBytes(result, "service_tier").Exists())
	require.False(t, gjson.GetBytes(result, "tools.0.input_schema.additionalProperties").Exists())
}
