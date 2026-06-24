package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opentalon/mcp-plugin/config"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// mutableMCPServer is a minimal Streamable HTTP MCP server whose single tool
// name the test can change between calls, to verify RefreshCapabilities
// re-fetches the upstream rather than serving the cached set.
func mutableMCPServer(t *testing.T) (*httptest.Server, func(string), func()) {
	t.Helper()
	var mu sync.Mutex
	tool := "old"
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result json.RawMessage
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
		case "tools/list":
			mu.Lock()
			name := tool
			mu.Unlock()
			result = json.RawMessage(fmt.Sprintf(`{"tools":[{"name":%q,"description":"t","inputSchema":{"type":"object","properties":{}}}]}`, name))
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
	var closeOnce sync.Once
	closeFn := func() { closeOnce.Do(srv.Close) }
	t.Cleanup(closeFn)
	return srv, func(n string) {
		mu.Lock()
		tool = n
		mu.Unlock()
	}, closeFn
}

func capsHasAction(caps pluginpkg.CapabilitiesMsg, name string) bool {
	for _, a := range caps.Actions {
		if a.Name == name {
			return true
		}
	}
	return false
}

// TestHandler_RefreshCapabilities_picksUpUpstreamChange verifies that
// Capabilities() is a pure read of the cache, while RefreshCapabilities()
// re-fetches the upstream and atomically swaps the cache.
func TestHandler_RefreshCapabilities_picksUpUpstreamChange(t *testing.T) {
	srv, setTool, _ := mutableMCPServer(t)
	ctx := testCtx(t)
	cfgs := []config.ServerConfig{{Server: "s", URL: srv.URL + "/mcp"}}

	reg, err := Build(ctx, cfgs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h := NewHandler(ctx)
	h.SetRegistry(reg, cfgs)

	if !capsHasAction(h.Capabilities(), "s__old") {
		t.Fatalf("expected s__old before refresh, got %+v", h.Capabilities().Actions)
	}

	// Upstream changes its tool. A plain Capabilities() must still serve the
	// cached old set (pure read, no network).
	setTool("new")
	if capsHasAction(h.Capabilities(), "s__new") {
		t.Errorf("Capabilities() must not re-fetch — should still show the cached old set")
	}

	// RefreshCapabilities re-fetches and returns the new set, and the swap is
	// visible to subsequent Capabilities() reads.
	fresh := h.RefreshCapabilities()
	if !capsHasAction(fresh, "s__new") {
		t.Errorf("RefreshCapabilities should return s__new, got %+v", fresh.Actions)
	}
	if capsHasAction(h.Capabilities(), "s__old") {
		t.Errorf("old action should be gone after refresh")
	}
	if !capsHasAction(h.Capabilities(), "s__new") {
		t.Errorf("Capabilities should reflect s__new after refresh, got %+v", h.Capabilities().Actions)
	}
}

// TestHandler_RefreshCapabilities_keepsPreviousOnDegradedUpstream verifies that a
// refresh which can't reach the upstream (degraded build) keeps the last good
// live capabilities instead of swapping in a stale/offline view — so a transient
// upstream blip doesn't churn the corpus.
func TestHandler_RefreshCapabilities_keepsPreviousOnDegradedUpstream(t *testing.T) {
	t.Setenv("OPENTALON_MCP_CACHE_DIR", "") // no cache: a down upstream yields a failed (empty) build, not a cached one
	srv, _, closeSrv := mutableMCPServer(t)
	ctx := testCtx(t)
	cfgs := []config.ServerConfig{{Server: "s", URL: srv.URL + "/mcp"}}

	reg, err := Build(ctx, cfgs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	h := NewHandler(ctx)
	h.SetRegistry(reg, cfgs)
	if !capsHasAction(h.Capabilities(), "s__old") {
		t.Fatalf("expected s__old before refresh, got %+v", h.Capabilities().Actions)
	}

	// Upstream goes down — the refresh can't get a live view.
	closeSrv()

	fresh := h.RefreshCapabilities()
	if !capsHasAction(fresh, "s__old") {
		t.Errorf("degraded refresh dropped the live caps (should keep them), got %+v", fresh.Actions)
	}
	if !capsHasAction(h.Capabilities(), "s__old") {
		t.Errorf("Capabilities lost the live caps after a degraded refresh, got %+v", h.Capabilities().Actions)
	}
}
