package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/mcp-plugin/config"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// fakeMCPServerCapturingCall serves initialize + tools/list (one tool with an
// open schema) and, on tools/call, records the request headers and the
// arguments the downstream tool actually received.
func fakeMCPServerCapturingCall(t *testing.T, toolName string, gotHeaders *http.Header, gotArgs *map[string]interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result json.RawMessage
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"` + toolName + `","description":"x","inputSchema":{"type":"object","properties":{}}}]}`)
		case "tools/call":
			if gotHeaders != nil {
				*gotHeaders = r.Header.Clone()
			}
			var p struct {
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if gotArgs != nil {
				*gotArgs = p.Arguments
			}
			result = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)
		default:
			result = json.RawMessage(`{}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int64           `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{JSONRPC: "2.0", ID: *req.ID, Result: result})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBuild_populatesInjectContextArgsFromContextHeaders(t *testing.T) {
	srv := fakeMCPServerCapturingCall(t, "do_thing", nil, nil)
	ctx := testCtx(t)

	cfg := config.ServerConfig{
		Server: "srv", URL: srv.URL + "/mcp",
		ContextHeaders: map[string]string{"session_id": "X-Session-Id", "actor_id": "X-Actor-Id"},
	}

	// Map iteration is randomized, so a single Build cannot tell a genuinely
	// sorted result from a lucky draw. Loop so that removing sort.Strings
	// would fail this test deterministically, not ~1-in-N.
	for i := 0; i < 40; i++ {
		r, err := Build(ctx, []config.ServerConfig{cfg})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(r.caps.Actions) == 0 {
			t.Fatal("no actions built")
		}
		for _, act := range r.caps.Actions {
			got := act.InjectContextArgs
			if len(got) != 2 || got[0] != "actor_id" || got[1] != "session_id" {
				t.Fatalf("run %d: %s InjectContextArgs = %v, want [actor_id session_id] (sorted)", i, act.Name, got)
			}
		}
	}

	// Backward compatible: no context_headers -> no injected args.
	cfg2 := config.ServerConfig{Server: "srv2", URL: srv.URL + "/mcp"}
	r2, err := Build(ctx, []config.ServerConfig{cfg2})
	if err != nil {
		t.Fatalf("Build (no context_headers): %v", err)
	}
	for _, act := range r2.caps.Actions {
		if len(act.InjectContextArgs) != 0 {
			t.Errorf("%s InjectContextArgs = %v, want empty", act.Name, act.InjectContextArgs)
		}
	}
}

func TestHandler_Execute_forwardsContextArgAsHeaderAndStripsArg(t *testing.T) {
	var headers http.Header
	var capturedArgs map[string]interface{}
	srv := fakeMCPServerCapturingCall(t, "do_thing", &headers, &capturedArgs)
	ctx := testCtx(t)

	cfg := config.ServerConfig{
		Server: "srv", URL: srv.URL + "/mcp",
		ContextHeaders: map[string]string{"session_id": "X-Session-Id"},
	}
	r, err := Build(ctx, []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h := NewHandler(ctx)
	h.SetRegistry(r, nil)

	// Simulate the host having injected session_id into req.Args (it does this
	// because the action declares it in InjectContextArgs).
	resp := h.Execute(pluginpkg.Request{
		ID:     "req-1",
		Action: "srv__do_thing",
		Args:   map[string]string{"session_id": "sess-abc", "foo": "bar"},
	})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	if headers.Get("X-Session-Id") != "sess-abc" {
		t.Errorf("X-Session-Id header = %q, want sess-abc", headers.Get("X-Session-Id"))
	}
	if _, leaked := capturedArgs["session_id"]; leaked {
		t.Errorf("session_id leaked into tool arguments: %v", capturedArgs)
	}
	if capturedArgs["foo"] != "bar" {
		t.Errorf("foo arg = %v, want bar", capturedArgs["foo"])
	}
}

func TestHandler_Execute_forwardsMultipleContextArgsAndStripsThem(t *testing.T) {
	var headers http.Header
	var capturedArgs map[string]interface{}
	srv := fakeMCPServerCapturingCall(t, "do_thing", &headers, &capturedArgs)
	ctx := testCtx(t)

	cfg := config.ServerConfig{
		Server: "srv", URL: srv.URL + "/mcp",
		ContextHeaders: map[string]string{"session_id": "X-Session-Id", "actor_id": "X-Actor-Id"},
	}
	r, err := Build(ctx, []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h := NewHandler(ctx)
	h.SetRegistry(r, nil)

	resp := h.Execute(pluginpkg.Request{
		ID:     "req-2",
		Action: "srv__do_thing",
		Args:   map[string]string{"session_id": "sess-1", "actor_id": "act-9", "foo": "bar"},
	})
	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}

	// Every mapped context arg is forwarded as its header...
	if headers.Get("X-Session-Id") != "sess-1" {
		t.Errorf("X-Session-Id = %q, want sess-1", headers.Get("X-Session-Id"))
	}
	if headers.Get("X-Actor-Id") != "act-9" {
		t.Errorf("X-Actor-Id = %q, want act-9", headers.Get("X-Actor-Id"))
	}
	// ...and every one is stripped from the tool arguments...
	for _, k := range []string{"session_id", "actor_id"} {
		if _, leaked := capturedArgs[k]; leaked {
			t.Errorf("%s leaked into tool arguments: %v", k, capturedArgs)
		}
	}
	// ...while a genuine argument still passes through.
	if capturedArgs["foo"] != "bar" {
		t.Errorf("foo arg = %v, want bar", capturedArgs["foo"])
	}
}
