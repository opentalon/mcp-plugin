package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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

// TestBuild_tracksFailedServerNoCache verifies that a server which is
// unreachable AND has no cache is added to failedServers (not silently dropped).
func TestBuild_tracksFailedServerNoCache(t *testing.T) {
	// No cache dir set → loadCache returns nil.
	t.Setenv("OPENTALON_MCP_CACHE_DIR", "")
	cfg := config.ServerConfig{Server: "gone", URL: "http://127.0.0.1:0/sse"}

	r, err := Build(context.Background(), []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(r.failedServers) != 1 || r.failedServers[0].Server != "gone" {
		t.Errorf("failedServers = %v, want [{gone ...}]", r.failedServers)
	}
	if len(r.actions) != 0 {
		t.Errorf("expected no actions for no-cache offline server, got %v", keys(r.actions))
	}
}

// TestBuild_doesNotTrackOfflineWithCache verifies that a server which is
// unreachable but HAS a cache is NOT in failedServers (it gets cached tools).
func TestBuild_doesNotTrackOfflineWithCache(t *testing.T) {
	t.Setenv("OPENTALON_MCP_CACHE_DIR", t.TempDir())
	cacheDir := config.CacheDir()
	_ = saveCache(cacheDir, "cached-srv", []mcp.Tool{{Name: "my_tool", Description: "cached"}})

	cfg := config.ServerConfig{Server: "cached-srv", URL: "http://127.0.0.1:0/sse"}
	r, err := Build(context.Background(), []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(r.failedServers) != 0 {
		t.Errorf("failedServers should be empty when cache is available, got %v", r.failedServers)
	}
	if _, ok := r.actions["cached-srv__my_tool"]; !ok {
		t.Errorf("expected cached tool in registry, got %v", keys(r.actions))
	}
}

// TestBuild_tracksOfflineServersWithCache verifies that a server which is
// unreachable but HAS a cache is tracked in offlineServers for background reconnection.
func TestBuild_tracksOfflineServersWithCache(t *testing.T) {
	t.Setenv("OPENTALON_MCP_CACHE_DIR", t.TempDir())
	cacheDir := config.CacheDir()
	_ = saveCache(cacheDir, "cached-srv", []mcp.Tool{{Name: "my_tool", Description: "cached"}})

	cfg := config.ServerConfig{Server: "cached-srv", URL: "http://127.0.0.1:0/sse"}
	r, err := Build(context.Background(), []config.ServerConfig{cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(r.offlineServers) != 1 || r.offlineServers[0].Server != "cached-srv" {
		t.Errorf("offlineServers = %v, want [{cached-srv ...}]", r.offlineServers)
	}
}

// TestStartBackgroundRetry_nopWhenEmpty verifies that StartBackgroundRetry
// is a no-op (does not panic, starts no goroutine) when failedServers is empty.
func TestStartBackgroundRetry_nopWhenEmpty(t *testing.T) {
	r := &Registry{actions: make(map[string]entry)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartBackgroundRetry(ctx) // must not panic or block
}

// TestRetryLoop_savesCacheWhenServerBecomesReachable verifies that when a
// previously-missing server becomes reachable, retryLoop writes its cache.
// A second server remains unreachable so os.Exit is never triggered, letting
// us verify the cache write without subprocess machinery.
func TestRetryLoop_savesCacheWhenServerBecomesReachable(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENTALON_MCP_CACHE_DIR", cacheDir)

	// Server A: real httptest server — will be reachable.
	srvA := fakeMCPHTTPServer(t, "alpha_tool")
	cfgA := config.ServerConfig{Server: "srvA", URL: srvA.URL + "/sse"}

	// Server B: permanently unreachable — keeps pending non-empty so exit is never called.
	cfgB := config.ServerConfig{Server: "srvB", URL: "http://127.0.0.1:0/sse"}

	r := &Registry{
		actions:       make(map[string]entry),
		failedServers: []config.ServerConfig{cfgA, cfgB},
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Run the retry loop in a goroutine; cancel after verifying.
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.retryLoop(ctx)
	}()

	// Poll for srvA's cache file (retry loop starts with 1 s backoff).
	deadline := make(chan struct{})
	go func() {
		for {
			data, err := os.ReadFile(cacheFile(cacheDir, "srvA"))
			if err == nil && len(data) > 0 {
				close(deadline)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	// Allow up to 5 s for the cache file to appear.
	select {
	case <-deadline:
		// success — cache was written
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for srvA cache to be written by retryLoop")
	}

	// srvB cache must NOT have been written.
	if _, err := os.ReadFile(cacheFile(cacheDir, "srvB")); err == nil {
		t.Error("srvB cache should not exist (server permanently unreachable)")
	}

	cancel()
	<-done
}

func keys(m map[string]entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
