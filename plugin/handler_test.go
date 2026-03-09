package plugin

import (
	"encoding/json"
	"testing"

	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

func TestMapType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"string", "string"},
		{"STRING", "string"},
		{"number", "number"},
		{"integer", "number"},
		{"boolean", "boolean"},
		{"object", "json"},
		{"array", "json"},
		{"unknown", "string"},
		{"", "string"},
	}
	for _, c := range cases {
		if got := mapType(c.in); got != c.want {
			t.Errorf("mapType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSchemaToParams_empty(t *testing.T) {
	if got := schemaToParams(mcp.InputSchema{}); got != nil {
		t.Errorf("want nil for empty schema, got %v", got)
	}
}

func TestSchemaToParams(t *testing.T) {
	schema := mcp.InputSchema{
		Properties: map[string]mcp.SchemaProp{
			"path":  {Type: "string", Description: "File path"},
			"count": {Type: "integer"},
			"data":  {Type: "object", Description: "Payload"},
		},
		Required: []string{"path"},
	}
	params := schemaToParams(schema)
	if len(params) != 3 {
		t.Fatalf("got %d params, want 3", len(params))
	}
	byName := make(map[string]pluginpkg.ParameterMsg)
	for _, p := range params {
		byName[p.Name] = p
	}
	if p := byName["path"]; p.Type != "string" || !p.Required || p.Description != "File path" {
		t.Errorf("path param: %+v", p)
	}
	if p := byName["count"]; p.Type != "number" || p.Required {
		t.Errorf("count param: %+v", p)
	}
	if p := byName["data"]; p.Type != "json" || p.Required {
		t.Errorf("data param: %+v", p)
	}
}

func TestCoerce(t *testing.T) {
	cases := []struct {
		v, typ string
		want   interface{}
	}{
		{"3.14", "number", float64(3.14)},
		{"not-a-number", "number", "not-a-number"},
		{"7", "integer", int64(7)},
		{"not-int", "integer", "not-int"},
		{"true", "boolean", true},
		{"false", "boolean", false},
		{"bad", "boolean", "bad"},
		{`{"k":"v"}`, "object", map[string]interface{}{"k": "v"}},
		{"bad-json", "object", "bad-json"},
		{`[1,2]`, "array", []interface{}{float64(1), float64(2)}},
		{"hello", "string", "hello"},
		{"hello", "", "hello"},
	}
	for _, c := range cases {
		got := coerce(c.v, c.typ)
		wantJ, _ := json.Marshal(c.want)
		gotJ, _ := json.Marshal(got)
		if string(gotJ) != string(wantJ) {
			t.Errorf("coerce(%q, %q) = %v (%T), want %v (%T)", c.v, c.typ, got, got, c.want, c.want)
		}
	}
}

func TestConvertArgs(t *testing.T) {
	schema := mcp.InputSchema{
		Properties: map[string]mcp.SchemaProp{
			"n":    {Type: "integer"},
			"flag": {Type: "boolean"},
			"data": {Type: "object"},
		},
	}
	result := convertArgs(map[string]string{
		"n":       "42",
		"flag":    "true",
		"data":    `{"x":1}`,
		"unknown": "raw",
	}, schema)
	if result["n"] != int64(42) {
		t.Errorf("n = %v (%T), want int64(42)", result["n"], result["n"])
	}
	if result["flag"] != true {
		t.Errorf("flag = %v, want true", result["flag"])
	}
	gotJ, _ := json.Marshal(result["data"])
	if string(gotJ) != `{"x":1}` {
		t.Errorf("data = %s, want {\"x\":1}", gotJ)
	}
	if result["unknown"] != "raw" {
		t.Errorf("unknown = %v, want raw", result["unknown"])
	}
}

func TestHandler_Execute_unknownAction(t *testing.T) {
	h := NewHandler(&Registry{actions: make(map[string]entry)})
	resp := h.Execute(pluginpkg.Request{ID: "req-1", Action: "does-not-exist"})
	if resp.Error == "" {
		t.Error("expected non-empty error for unknown action")
	}
	if resp.CallID != "req-1" {
		t.Errorf("CallID = %q, want req-1", resp.CallID)
	}
}

func TestHandler_Capabilities(t *testing.T) {
	r := &Registry{
		actions: make(map[string]entry),
		caps: pluginpkg.CapabilitiesMsg{
			Name:        "mcp",
			Description: "test",
			Actions: []pluginpkg.ActionMsg{
				{Name: "s__tool", Description: "A tool"},
			},
		},
	}
	h := NewHandler(r)
	caps := h.Capabilities()
	if caps.Name != "mcp" {
		t.Errorf("Name = %q, want mcp", caps.Name)
	}
	if len(caps.Actions) != 1 || caps.Actions[0].Name != "s__tool" {
		t.Errorf("unexpected actions: %+v", caps.Actions)
	}
}
