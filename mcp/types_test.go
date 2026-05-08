package mcp

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSchemaProp_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		wantType     string
		wantNullable bool
	}{
		{
			name:     "string type",
			input:    `{"type":"string","description":"a field"}`,
			wantType: "string",
		},
		{
			name:         "array type picks first non-null",
			input:        `{"type":["string","null"]}`,
			wantType:     "string",
			wantNullable: true,
		},
		{
			name:         "array type null first picks second",
			input:        `{"type":["null","integer"]}`,
			wantType:     "integer",
			wantNullable: true,
		},
		{
			name:         "array type all null picks first",
			input:        `{"type":["null","null"]}`,
			wantType:     "null",
			wantNullable: true,
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
			if prop.Nullable != tc.wantNullable {
				t.Errorf("Nullable = %v, want %v", prop.Nullable, tc.wantNullable)
			}
		})
	}
}

func TestRpcError_String(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no data field — falls back to message only",
			in:   `{"code":-32602,"message":"Invalid params"}`,
			want: "Invalid params",
		},
		{
			name: "data is a JSON string — appended after message, unquoted",
			in:   `{"code":-32602,"message":"Invalid params","data":"Missing required arguments: item_id"}`,
			want: "Invalid params: Missing required arguments: item_id",
		},
		{
			name: "data is a JSON object — appended raw, kept inspectable",
			in:   `{"code":-32603,"message":"Internal error","data":{"backtrace_id":"abc","kind":"validation_error"}}`,
			want: `Internal error: {"backtrace_id":"abc","kind":"validation_error"}`,
		},
		{
			name: "empty data string still falls back to message only",
			in:   `{"code":-32602,"message":"Invalid params","data":""}`,
			want: "Invalid params: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e rpcError
			if err := json.Unmarshal([]byte(tc.in), &e); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := e.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			// Format-via-Stringer through *rpcError matches the call sites
			// (`resp.Error` on rpcResponse is a pointer). Auto-deref must
			// pick up the value-receiver String — guard against accidental
			// future move to pointer-receiver only that would silently
			// regress the %s path used in client.go's error wrapping.
			// Deliberately Sprintf here (not .String()): we are testing the
			// Stringer-via-fmt path that the actual fmt.Errorf call sites
			// take. Calling .String() directly would not catch a regression
			// where the receiver type changes and auto-deref no longer fires.
			ptr := &e
			if got := fmt.Sprintf("%s", ptr); got != tc.want { //nolint:staticcheck // S1025 — testing fmt-Stringer path on purpose, see comment above
				t.Errorf("Sprintf via *rpcError = %q, want %q", got, tc.want)
			}
		})
	}
}
