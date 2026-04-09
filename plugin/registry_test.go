package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/mcp-plugin/config"
	"github.com/opentalon/mcp-plugin/mcp"
)

// fakeMCPHTTPServer creates a minimal SSE+POST MCP server returning one tool.
func fakeMCPHTTPServer(t *testing.T, toolName string) *httptest.Server {
	t.Helper()
	events := make(chan string, 16)
	mux := http.NewServeMux()

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /message\n\n")
		fl.Flush()
		for {
			select {
			case ev := <-events:
				_, _ = fmt.Fprint(w, ev)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if req.ID == nil {
			return
		}

		// One JSON-RPC result per POST, with the same id the client subscribed for.
		// Pushing both initialize and tools/list on every request causes the second
		// SSE event to be dispatched before ListTools calls subscribe (response lost).
		var result json.RawMessage
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
		case "tools/list":
			result = json.RawMessage(fmt.Sprintf(`{"tools":[{"name":%q,"description":"test tool","inputSchema":{"type":"object","properties":{}}}]}`, toolName))
		default:
			result = json.RawMessage(`{}`)
		}
		body, err := json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int64           `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{
			JSONRPC: "2.0",
			ID:      *req.ID,
			Result:  result,
		})
		if err != nil {
			return
		}
		events <- "data: " + string(body) + "\n\n"
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// testCtx returns a context cancelled during t.Cleanup before httptest.Server.Close.
// That tears down SSE GETs so srv.Close does not block (same pattern as mcp/client_test.go).
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestBuild_populatesRegistry(t *testing.T) {
	srv := fakeMCPHTTPServer(t, "do_thing")
	ctx := testCtx(t)
	cfg := config.ServerConfig{Server: "testsrv", URL: srv.URL + "/sse"}

	r, err := Build(ctx, []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := r.actions["testsrv__do_thing"]; !ok {
		t.Errorf("expected action testsrv__do_thing in registry, got %v", keys(r.actions))
	}
	if len(r.caps.Actions) != 1 {
		t.Errorf("caps.Actions len = %d, want 1", len(r.caps.Actions))
	}
}

func TestBuild_usesServerKeyInActionName(t *testing.T) {
	// Verifies that the action name is "<server>__<tool>", not "<name>__<tool>".
	// This guards against the mcp_inject bug where "name" was used instead of "server".
	srv := fakeMCPHTTPServer(t, "get_info")
	ctx := testCtx(t)
	cfg := config.ServerConfig{Server: "appsignal", URL: srv.URL + "/sse"}

	r, err := Build(ctx, []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := r.actions["appsignal__get_info"]; !ok {
		t.Errorf("expected action appsignal__get_info, got %v", keys(r.actions))
	}
}

func TestBuild_fallsBackToCacheOnConnectFailure(t *testing.T) {
	// Use an unreachable URL; supply a pre-seeded cache via env.
	t.Setenv("OPENTALON_MCP_CACHE_DIR", t.TempDir())
	cacheDir := config.CacheDir()

	// Seed the cache manually.
	_ = saveCache(cacheDir, "offline-srv", []mcp.Tool{
		{Name: "cached_tool", Description: "from cache"},
	})

	cfg := config.ServerConfig{Server: "offline-srv", URL: "http://127.0.0.1:0/sse"}
	r, err := Build(context.Background(), []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := r.actions["offline-srv__cached_tool"]; !ok {
		t.Errorf("expected cached_tool in registry, got %v", keys(r.actions))
	}
}

func TestBuild_emptyServerNameProducesDoubleUnderscorePrefix(t *testing.T) {
	// Confirms that an empty Server name produces an action like "__toolname",
	// which is the observable symptom of the mcp_inject "name" vs "server" bug.
	// This test documents the behaviour so the fix in mcp_inject is meaningful.
	srv := fakeMCPHTTPServer(t, "my_tool")
	ctx := testCtx(t)
	cfg := config.ServerConfig{Server: "", URL: srv.URL + "/sse"}

	r, err := Build(ctx, []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := r.actions["__my_tool"]; !ok {
		t.Errorf("expected __my_tool when server name is empty, got %v", keys(r.actions))
	}
}

func keys(m map[string]entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
