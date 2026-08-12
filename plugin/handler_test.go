package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

func TestSchemaToParams_empty(t *testing.T) {
	if got := schemaToParams(mcp.InputSchema{}); got != nil {
		t.Errorf("want nil for empty schema, got %v", got)
	}
}

// TestSchemaToParams pins the whole point of the bridge's parameter handling:
// each property reaches the host as the server wrote it. An enum is the case
// that matters — a parameter whose allowed values live only in its enum is a
// parameter the model has to guess if the bridge reduces the property to a
// type name on the way past. Name, description and requiredness are still
// derived here, because the host needs them outside the fragment.
func TestSchemaToParams(t *testing.T) {
	const (
		pathProp  = `{"type":"string","description":"File path"}`
		kindProp  = `{"type":"string","enum":["asset","consumable"],"description":"Which kind"}`
		tagsProp  = `{"type":"array","items":{"type":"string"}}`
		countProp = `{"type":["integer","null"]}`
	)
	var schema mcp.InputSchema
	if err := json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {
			"path":  `+pathProp+`,
			"kind":  `+kindProp+`,
			"tags":  `+tagsProp+`,
			"count": `+countProp+`
		},
		"required": ["path"]
	}`), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	params := schemaToParams(schema)
	if len(params) != 4 {
		t.Fatalf("got %d params, want 4", len(params))
	}
	byName := make(map[string]pluginpkg.ParameterMsg)
	for _, p := range params {
		byName[p.Name] = p
	}

	if p := byName["path"]; !p.Required || p.Description != "File path" || string(p.Schema) != pathProp {
		t.Errorf("path param: %+v", p)
	}
	if p := byName["kind"]; p.Required || string(p.Schema) != kindProp {
		t.Errorf("kind param: %+v — the enum must survive verbatim", p)
	}
	if p := byName["tags"]; string(p.Schema) != tagsProp {
		t.Errorf("tags param: %+v — the item type must survive verbatim", p)
	}
	if p := byName["count"]; string(p.Schema) != countProp {
		t.Errorf("count param: %+v — the nullable type union must survive verbatim", p)
	}
}

// TestSchemaToParams_noRawJSON covers a schema assembled in code rather than
// parsed from a server: there is no fragment to pass on, so the parameter
// carries only what the struct holds and the host synthesises the rest.
func TestSchemaToParams_noRawJSON(t *testing.T) {
	params := schemaToParams(mcp.InputSchema{
		Properties: map[string]mcp.SchemaProp{
			"path": {Type: "string", Description: "File path"},
		},
		Required: []string{"path"},
	})
	if len(params) != 1 {
		t.Fatalf("got %d params, want 1", len(params))
	}
	if p := params[0]; p.Name != "path" || !p.Required || p.Description != "File path" || len(p.Schema) != 0 {
		t.Errorf("path param: %+v, want no schema fragment", p)
	}
}

func TestCoerce(t *testing.T) {
	cases := []struct {
		v        string
		typ      string
		nullable bool
		want     interface{}
	}{
		{"3.14", "number", false, float64(3.14)},
		{"not-a-number", "number", false, "not-a-number"},
		{"7", "integer", false, int64(7)},
		{"not-int", "integer", false, "not-int"},
		{"true", "boolean", false, true},
		{"false", "boolean", false, false},
		{"bad", "boolean", false, "bad"},
		{`{"k":"v"}`, "object", false, map[string]interface{}{"k": "v"}},
		{"bad-json", "object", false, "bad-json"},
		{`[1,2]`, "array", false, []interface{}{float64(1), float64(2)}},
		{"hello", "string", false, "hello"},
		{"hello", "", false, "hello"},
		// nullable: "null" string → nil
		{"null", "integer", true, nil},
		{"null", "string", true, nil},
		{"null", "number", true, nil},
		// nullable but value is not "null" → normal coercion
		{"42", "integer", true, int64(42)},
		// not nullable: "null" stays as string
		{"null", "integer", false, "null"},
	}
	for _, c := range cases {
		got := coerce(c.v, c.typ, c.nullable)
		wantJ, _ := json.Marshal(c.want)
		gotJ, _ := json.Marshal(got)
		if string(gotJ) != string(wantJ) {
			t.Errorf("coerce(%q, %q, nullable=%v) = %v (%T), want %v (%T)", c.v, c.typ, c.nullable, got, got, c.want, c.want)
		}
	}
}

func TestConvertArgs(t *testing.T) {
	schema := mcp.InputSchema{
		Properties: map[string]mcp.SchemaProp{
			"n":         {Type: "integer"},
			"flag":      {Type: "boolean"},
			"data":      {Type: "object"},
			"parent_id": {Type: "integer", Nullable: true},
		},
	}
	result := convertArgs(map[string]string{
		"n":         "42",
		"flag":      "true",
		"data":      `{"x":1}`,
		"unknown":   "raw",
		"parent_id": "null",
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
	if result["parent_id"] != nil {
		t.Errorf("parent_id = %v (%T), want nil", result["parent_id"], result["parent_id"])
	}
}

func TestHandler_Execute_unknownAction(t *testing.T) {
	h := NewHandler(context.Background())
	h.SetRegistry(&Registry{actions: make(map[string]entry)}, nil)
	resp := h.Execute(pluginpkg.Request{ID: "req-1", Action: "does-not-exist"})
	if resp.Error == "" {
		t.Error("expected non-empty error for unknown action")
	}
	if resp.CallID != "req-1" {
		t.Errorf("CallID = %q, want req-1", resp.CallID)
	}
}

func TestResolveToolNameArgs(t *testing.T) {
	h := NewHandler(context.Background())
	// Registered as "<server>__<tool>"; the raw MCP name is "update-item".
	h.SetRegistry(&Registry{
		actions: map[string]entry{
			"timly__update-item": {mcpToolName: "update-item"},
		},
	}, nil)

	taskToolName := func(args map[string]interface{}, i int) interface{} {
		return args["tasks"].([]interface{})[i].(map[string]interface{})["tool_name"]
	}

	t.Run("canonical double-prefixed FQN in a task resolves to the raw name", func(t *testing.T) {
		// The LLM sees and emits "timly__timly__update-item"; the server wants "update-item".
		args := map[string]interface{}{
			"tasks": []interface{}{
				map[string]interface{}{"tool_name": "timly__timly__update-item", "id": 1},
			},
		}
		h.resolveToolNameArgs(args)
		if got := taskToolName(args, 0); got != "update-item" {
			t.Errorf("tool_name = %v, want update-item", got)
		}
	})

	t.Run("action key resolves and a bare name passes through unchanged", func(t *testing.T) {
		args := map[string]interface{}{
			"tasks": []interface{}{
				map[string]interface{}{"tool_name": "timly__update-item"},
				map[string]interface{}{"tool_name": "update-item"},
			},
		}
		h.resolveToolNameArgs(args)
		if got := taskToolName(args, 0); got != "update-item" {
			t.Errorf("action-key tool_name = %v, want update-item", got)
		}
		if got := taskToolName(args, 1); got != "update-item" {
			t.Errorf("bare tool_name = %v, want update-item (unchanged)", got)
		}
	})

	t.Run("default_tool_name and the top-level tool_name filter resolve the canonical FQN", func(t *testing.T) {
		args := map[string]interface{}{
			"default_tool_name": "timly__timly__update-item",
			"tool_name":         "timly__timly__update-item",
		}
		h.resolveToolNameArgs(args)
		if got := args["default_tool_name"]; got != "update-item" {
			t.Errorf("default_tool_name = %v, want update-item", got)
		}
		if got := args["tool_name"]; got != "update-item" {
			t.Errorf("tool_name = %v, want update-item", got)
		}
	})

	t.Run("a value resolving to no known tool is left untouched", func(t *testing.T) {
		args := map[string]interface{}{"tool_name": "timly__timly__not-a-tool"}
		h.resolveToolNameArgs(args)
		if got := args["tool_name"]; got != "timly__timly__not-a-tool" {
			t.Errorf("unknown tool_name = %v, want unchanged", got)
		}
	})
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
	h := NewHandler(context.Background())
	h.SetRegistry(r, nil)
	caps := h.Capabilities()
	if caps.Name != "mcp" {
		t.Errorf("Name = %q, want mcp", caps.Name)
	}
	if len(caps.Actions) != 1 || caps.Actions[0].Name != "s__tool" {
		t.Errorf("unexpected actions: %+v", caps.Actions)
	}
}

// Nil-registry guard tests — before Configure or SetRegistry is called.

func TestHandler_nilRegistry_Capabilities(t *testing.T) {
	h := NewHandler(context.Background())
	caps := h.Capabilities()
	if caps.Name != "mcp" {
		t.Errorf("Name = %q, want mcp", caps.Name)
	}
	if len(caps.Actions) != 0 {
		t.Errorf("expected no actions before Configure, got %d", len(caps.Actions))
	}
}

func TestHandler_nilRegistry_Execute(t *testing.T) {
	h := NewHandler(context.Background())
	resp := h.Execute(pluginpkg.Request{ID: "req-1", Action: "any"})
	if resp.Error == "" {
		t.Fatal("expected error from Execute before Configure")
	}
	if !strings.Contains(resp.Error, "not yet configured") {
		t.Errorf("error = %q, want it to contain 'not yet configured'", resp.Error)
	}
	if resp.CallID != "req-1" {
		t.Errorf("CallID = %q, want req-1", resp.CallID)
	}
}

// Configure tests.

func TestHandler_Configure_malformedJSON(t *testing.T) {
	h := NewHandler(context.Background())
	if err := h.Configure("not-json"); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestHandler_Configure_noServers(t *testing.T) {
	h := NewHandler(context.Background())
	if err := h.Configure(`{"servers":[]}`); err == nil {
		t.Fatal("expected error for empty servers list")
	}
}

// TestHandler_Configure_setsRegistry verifies that after a successful Configure
// call the registry is non-nil. The server URL is intentionally unreachable;
// Build logs the failure and returns an empty registry without an error, so
// Configure should still set h.registry. We detect this by checking that
// Execute returns "unknown action" rather than "not yet configured".
func TestHandler_Configure_setsRegistry(t *testing.T) {
	h := NewHandler(context.Background())
	err := h.Configure(`{"servers":[{"server":"s1","url":"http://127.0.0.1:0/sse"}]}`)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	resp := h.Execute(pluginpkg.Request{ID: "r1", Action: "s1__tool"})
	if strings.Contains(resp.Error, "not yet configured") {
		t.Errorf("registry was not set after Configure; error = %q", resp.Error)
	}
}
