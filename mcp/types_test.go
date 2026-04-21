package mcp

import (
	"encoding/json"
	"testing"
)

func TestSchemaProp_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantType string
	}{
		{
			name:     "string type",
			input:    `{"type":"string","description":"a field"}`,
			wantType: "string",
		},
		{
			name:     "array type picks first non-null",
			input:    `{"type":["string","null"]}`,
			wantType: "string",
		},
		{
			name:     "array type null first picks second",
			input:    `{"type":["null","integer"]}`,
			wantType: "integer",
		},
		{
			name:     "array type all null picks first",
			input:    `{"type":["null","null"]}`,
			wantType: "null",
		},
		{
			name:     "missing type",
			input:    `{"description":"no type"}`,
			wantType: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prop SchemaProp
			if err := json.Unmarshal([]byte(tc.input), &prop); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if prop.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", prop.Type, tc.wantType)
			}
		})
	}
}
