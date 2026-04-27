package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/mcp-plugin/config"
)

// fakeMCPServer is a minimal HTTP server implementing the MCP SSE protocol.
// GET /sse sends the endpoint event then streams JSON-RPC responses.
// POST /message receives JSON-RPC requests and responds via SSE.
type fakeMCPServer struct {
	srv      *httptest.Server
	eventsCh chan string // formatted SSE events ready to write
}

func newFakeMCPServer(t *testing.T) *fakeMCPServer {
	t.Helper()
	fs := &fakeMCPServer{
		eventsCh: make(chan string, 32),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", fs.sseHandler)
	mux.HandleFunc("/message", fs.messageHandler(t))
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeMCPServer) cfg() config.ServerConfig {
	return config.ServerConfig{Server: "test", URL: fs.srv.URL + "/sse"}
}

func (fs *fakeMCPServer) sseHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /message\n\n")
	fl.Flush()
	for {
		select {
		case ev := <-fs.eventsCh:
			_, _ = fmt.Fprint(w, ev)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (fs *fakeMCPServer) messageHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if req.ID == nil {
			return // notification — no response needed
		}
		go fs.respond(t, req)
	}
}

func (fs *fakeMCPServer) respond(t *testing.T, req rpcRequest) {
	t.Helper()
	var result json.RawMessage
	switch req.Method {
	case "initialize":
		result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
	case "tools/list":
		result = json.RawMessage(`{"tools":[
			{"name":"echo","description":"Echo input","inputSchema":{
				"type":"object",
				"properties":{"text":{"type":"string","description":"Text to echo"}},
				"required":["text"]
			}}
		]}`)
	case "tools/call":
		b, _ := json.Marshal(req.Params)
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		_ = json.Unmarshal(b, &p)
		if p.Name == "echo" {
			text, _ := p.Arguments["text"].(string)
			r, _ := json.Marshal(toolsCallResult{
				Content: []Content{{Type: "text", Text: text}},
			})
			result = r
		} else {
			r, _ := json.Marshal(toolsCallResult{
				IsError: true,
				Content: []Content{{Type: "text", Text: "unknown tool: " + p.Name}},
			})
			result = r
		}
	default:
		result = json.RawMessage(`{}`)
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result}
	data, _ := json.Marshal(resp)
	fs.eventsCh <- "data: " + string(data) + "\n\n"
}

// fakeStreamableMCPServer is a minimal MCP server using the Streamable HTTP
// protocol. POST /mcp receives JSON-RPC, responds with JSON directly.
type fakeStreamableMCPServer struct {
	srv *httptest.Server
}

func newFakeStreamableMCPServer(t *testing.T) *fakeStreamableMCPServer {
	t.Helper()
	fs := &fakeStreamableMCPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", fs.handleMCP(t))
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeStreamableMCPServer) cfg() config.ServerConfig {
	return config.ServerConfig{Server: "test-streamable", URL: fs.srv.URL + "/mcp"}
}

func (fs *fakeStreamableMCPServer) handleMCP(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			// Notification — no response needed.
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result json.RawMessage
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
		case "tools/list":
			result = json.RawMessage(`{"tools":[
				{"name":"echo","description":"Echo input","inputSchema":{
					"type":"object",
					"properties":{"text":{"type":"string","description":"Text to echo"}},
					"required":["text"]
				}}
			]}`)
		case "tools/call":
			b, _ := json.Marshal(req.Params)
			var p struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(b, &p)
			if p.Name == "echo" {
				text, _ := p.Arguments["text"].(string)
				r, _ := json.Marshal(toolsCallResult{
					Content: []Content{{Type: "text", Text: text}},
				})
				result = r
			} else {
				r, _ := json.Marshal(toolsCallResult{
					IsError: true,
					Content: []Content{{Type: "text", Text: "unknown tool: " + p.Name}},
				})
				result = r
			}
		default:
			result = json.RawMessage(`{}`)
		}

		resp := rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session-123")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// Tests

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		sseURL, endpoint, want string
	}{
		{"http://host/sse", "/msg?id=1", "http://host/msg?id=1"},
		{"http://host/sse", "http://other/msg", "http://other/msg"},
		{"http://host:8080/sse", "/msg", "http://host:8080/msg"},
		{"http://host/sse", "/msg", "http://host/msg"},
	}
	for _, c := range cases {
		got := resolveEndpoint(c.sseURL, c.endpoint)
		if got != c.want {
			t.Errorf("resolveEndpoint(%q, %q) = %q, want %q", c.sseURL, c.endpoint, got, c.want)
		}
	}
}

// testCtx returns a context that is cancelled during t.Cleanup, before the
// fake server closes. This ensures the SSE connection is torn down cleanly.
// Cleanup order is LIFO: cancel (registered here) runs before srv.Close
// (registered in newFakeMCPServer), so the server can shut down without hanging.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestClient_Connect(t *testing.T) {
	srv := newFakeMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestClient_ListTools(t *testing.T) {
	srv := newFakeMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want echo", tools[0].Name)
	}
	if tools[0].InputSchema.Properties["text"].Type != "string" {
		t.Errorf("text param type = %q, want string", tools[0].InputSchema.Properties["text"].Type)
	}
}

func TestClient_CallTool_success(t *testing.T) {
	srv := newFakeMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got, _, err := c.CallTool("echo", map[string]interface{}{"text": "hello world"}, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "hello world" {
		t.Errorf("content = %q, want %q", got, "hello world")
	}
}

func TestClient_CallTool_toolError(t *testing.T) {
	srv := newFakeMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err := c.CallTool("unknown-tool", map[string]interface{}{}, nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

// TestClient_CallTool_extraHeaders verifies that per-request credential headers
// are sent to the upstream MCP server and override static config headers.
func TestClient_CallTool_extraHeaders(t *testing.T) {
	var capturedHeaders http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Capture headers from the tools/call request.
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "tools/call" {
			capturedHeaders = r.Header.Clone()
		}
		var result json.RawMessage
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`)
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"echo","description":"Echo","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}`)
		case "tools/call":
			result = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)
		default:
			result = json.RawMessage(`{}`)
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.ServerConfig{
		Server:  "timly",
		URL:     srv.URL + "/mcp",
		Headers: map[string]string{"X-Static": "from-config"},
	}
	c := NewClient(cfg)
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	extra := http.Header{}
	extra.Set("X-Timly-User", "user-123")
	extra.Set("X-Static", "from-whoami") // should override static

	_, _, err := c.CallTool("echo", map[string]interface{}{"text": "hi"}, extra)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if capturedHeaders == nil {
		t.Fatal("tools/call request was not captured")
	}
	if got := capturedHeaders.Get("X-Timly-User"); got != "user-123" {
		t.Errorf("X-Timly-User = %q, want user-123", got)
	}
	if got := capturedHeaders.Get("X-Static"); got != "from-whoami" {
		t.Errorf("X-Static = %q, want from-whoami (credential headers should override static)", got)
	}
}

func TestClient_ServerName(t *testing.T) {
	c := NewClient(config.ServerConfig{Server: "myserver", URL: "http://x/sse"})
	if got := c.ServerName(); got != "myserver" {
		t.Errorf("ServerName = %q, want myserver", got)
	}
}

// --- Streamable HTTP transport tests ---

func TestStreamable_Connect(t *testing.T) {
	srv := newFakeStreamableMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Verify it chose Streamable HTTP (not SSE).
	if _, ok := c.tp.(*streamableHTTP); !ok {
		t.Errorf("expected streamableHTTP transport, got %T", c.tp)
	}
}

func TestStreamable_ListTools(t *testing.T) {
	srv := newFakeStreamableMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want echo", tools[0].Name)
	}
}

func TestStreamable_CallTool_success(t *testing.T) {
	srv := newFakeStreamableMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got, _, err := c.CallTool("echo", map[string]interface{}{"text": "streamable hello"}, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "streamable hello" {
		t.Errorf("content = %q, want %q", got, "streamable hello")
	}
}

func TestStreamable_CallTool_toolError(t *testing.T) {
	srv := newFakeStreamableMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err := c.CallTool("unknown-tool", map[string]interface{}{}, nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestStreamable_SessionID(t *testing.T) {
	srv := newFakeStreamableMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, ok := c.tp.(*streamableHTTP)
	if !ok {
		t.Fatalf("expected streamableHTTP transport, got %T", c.tp)
	}
	if st.sessionID != "test-session-123" {
		t.Errorf("sessionID = %q, want test-session-123", st.sessionID)
	}
}

// --- SSE-framed Streamable HTTP server (FastMCP / mcp-atlassian style) ---
//
// Some MCP servers (e.g. FastMCP) respond to Streamable HTTP POSTs with
// Content-Type: text/event-stream even for synchronous round-trips.  The body
// is SSE-framed: "event: message\ndata: {…}\n\n".  These tests exercise that
// path end-to-end and verify that reconnects work (bug #2).

type fakeSSEFramedStreamableServer struct {
	srv *httptest.Server
}

func newFakeSSEFramedStreamableServer(t *testing.T) *fakeSSEFramedStreamableServer {
	t.Helper()
	fs := &fakeSSEFramedStreamableServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", fs.handleMCP(t))
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeSSEFramedStreamableServer) cfg() config.ServerConfig {
	return config.ServerConfig{Server: "test-sse-framed", URL: fs.srv.URL + "/mcp"}
}

func (fs *fakeSSEFramedStreamableServer) handleMCP(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Require the correct Accept header (bug #1 guard).
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "text/event-stream") {
			http.Error(w, "406 Not Acceptable", http.StatusNotAcceptable)
			return
		}

		var req rpcRequest
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
			result = json.RawMessage(`{"tools":[
				{"name":"echo","description":"Echo input","inputSchema":{
					"type":"object",
					"properties":{"text":{"type":"string","description":"Text to echo"}},
					"required":["text"]
				}}
			]}`)
		case "tools/call":
			b, _ := json.Marshal(req.Params)
			var p struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(b, &p)
			text, _ := p.Arguments["text"].(string)
			r, _ := json.Marshal(toolsCallResult{
				Content: []Content{{Type: "text", Text: text}},
			})
			result = r
		default:
			result = json.RawMessage(`{}`)
		}

		resp := rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result}
		data, _ := json.Marshal(resp)

		// Respond with SSE framing — same as FastMCP / mcp-atlassian.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sse-framed-session")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	}
}

func TestStreamableSSEFramed_Connect(t *testing.T) {
	srv := newFakeSSEFramedStreamableServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, ok := c.tp.(*streamableHTTP); !ok {
		t.Errorf("expected streamableHTTP transport, got %T", c.tp)
	}
}

func TestStreamableSSEFramed_ListTools(t *testing.T) {
	srv := newFakeSSEFramedStreamableServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Errorf("unexpected tools: %+v", tools)
	}
}

func TestStreamableSSEFramed_CallTool(t *testing.T) {
	srv := newFakeSSEFramedStreamableServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got, _, err := c.CallTool("echo", map[string]interface{}{"text": "fastmcp hello"}, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got != "fastmcp hello" {
		t.Errorf("content = %q, want %q", got, "fastmcp hello")
	}
}

// TestStreamableSSEFramed_Reconnect verifies that a second Connect() call on a
// fresh client succeeds — this was bug #2 where retries always reported
// "Streamable HTTP unavailable" because SSE framing was never decoded.
func TestStreamableSSEFramed_Reconnect(t *testing.T) {
	srv := newFakeSSEFramedStreamableServer(t)
	ctx := testCtx(t)

	for i := range 2 {
		c := NewClient(srv.cfg())
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect attempt %d: %v", i+1, err)
		}
		if _, ok := c.tp.(*streamableHTTP); !ok {
			t.Errorf("attempt %d: expected streamableHTTP transport, got %T", i+1, c.tp)
		}
	}
}

// TestStreamableSSEFramed_AcceptHeader verifies that the client sends the
// Accept: application/json, text/event-stream header (bug #1).  The fake
// server returns 406 if the header is absent or wrong.
func TestStreamableSSEFramed_AcceptHeader(t *testing.T) {
	srv := newFakeSSEFramedStreamableServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect failed (likely missing Accept header): %v", err)
	}
}

// --- Retry test: Streamable HTTP returns EOF on first attempt, succeeds on retry ---

func TestStreamable_RetryOnEOF(t *testing.T) {
	// Simulate supergateway's double-res.end() bug: first request gets
	// connection closed (EOF), second request succeeds normally.
	var attempt int
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		attempt++
		if attempt == 1 {
			// Simulate EOF: hijack the connection and close it without sending a response.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support Hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("Hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		// Normal response on retry.
		var req rpcRequest
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
		default:
			result = json.RawMessage(`{}`)
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(config.ServerConfig{Server: "retry-test", URL: srv.URL + "/mcp"})
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect should succeed after retry, got: %v", err)
	}
	if _, ok := c.tp.(*streamableHTTP); !ok {
		t.Errorf("expected streamableHTTP transport after retry, got %T", c.tp)
	}
	if attempt < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempt)
	}
}

// --- Fallback test: Streamable HTTP fails (POST returns 404), falls back to SSE ---

func TestFallback_StreamableToSSE(t *testing.T) {
	// Use the SSE-only fake server. Its /sse endpoint handles GET (SSE)
	// and /message handles POST. A POST to /sse will not match the
	// Streamable HTTP protocol, so the client should fall back to SSE.
	srv := newFakeMCPServer(t)
	c := NewClient(srv.cfg())
	if err := c.Connect(testCtx(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Verify it fell back to SSE.
	if _, ok := c.tp.(*sseTransport); !ok {
		t.Errorf("expected sseTransport (fallback), got %T", c.tp)
	}
	// Verify it still works.
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Errorf("unexpected tools: %+v", tools)
	}
}

// TestFallbackSSE_ListToolsFails tests that FallbackSSE works when StreamableHTTP
// initialize succeeds but tools/list returns 503 (server only truly supports SSE).
func TestFallbackSSE_ListToolsFails(t *testing.T) {
	// Build a hybrid server: POST to /sse returns a valid StreamableHTTP
	// initialize response but 503 for tools/list. GET to /sse and POST to
	// /message implement the normal SSE protocol.
	fs := &fakeMCPServer{eventsCh: make(chan string, 32)}
	mux := http.NewServeMux()

	// SSE GET handler (normal SSE protocol).
	mux.HandleFunc("GET /sse", fs.sseHandler)

	// POST /sse — respond to initialize (so StreamableHTTP auto-detect succeeds)
	// but return 503 for anything else (e.g. tools/list).
	mux.HandleFunc("POST /sse", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			// notification (e.g. notifications/initialized) — accept
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "initialize" {
			resp := rpcResponse{
				JSONRPC: "2.0",
				ID:      float64(*req.ID),
				Result:  json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{}}`),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Everything else (tools/list, tools/call) → 503
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	})

	// POST /message — normal SSE message handler.
	mux.HandleFunc("POST /message", fs.messageHandler(t))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(config.ServerConfig{Server: "hybrid-test", URL: srv.URL + "/sse"})
	ctx := testCtx(t)

	// Connect should succeed via StreamableHTTP (initialize works).
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !c.IsStreamableHTTP() {
		t.Fatalf("expected StreamableHTTP after connect, got %T", c.tp)
	}

	// ListTools should fail (503).
	_, err2 := c.ListTools()
	if err2 == nil {
		t.Fatal("expected ListTools to fail on StreamableHTTP, but it succeeded")
	}
	if !strings.Contains(err2.Error(), "503") {
		t.Fatalf("expected 503 error, got: %v", err2)
	}

	// FallbackSSE should reconnect via SSE and work.
	if err := c.FallbackSSE(ctx); err != nil {
		t.Fatalf("FallbackSSE: %v", err)
	}
	if c.IsStreamableHTTP() {
		t.Fatal("expected SSE transport after fallback, still StreamableHTTP")
	}

	// ListTools should now succeed via SSE.
	tools2, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools after SSE fallback: %v", err)
	}
	if len(tools2) != 1 || tools2[0].Name != "echo" {
		t.Errorf("unexpected tools: %+v", tools2)
	}
}

// initWithInstructionsServer is a minimal Streamable HTTP MCP server whose
// initialize response includes an `instructions` field. Used to verify the
// client captures it.
func initWithInstructionsServer(t *testing.T, instructions string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
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
			payload := map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{},
			}
			if instructions != "" {
				payload["instructions"] = instructions
			}
			b, _ := json.Marshal(payload)
			result = b
		case "tools/list":
			result = json.RawMessage(`{"tools":[]}`)
		default:
			result = json.RawMessage(`{}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: float64(*req.ID), Result: result})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_CapturesInitializeInstructions(t *testing.T) {
	const prose = "Timly MCP server.\n\n## Org-units vs Containers\nA Place is a storage unit; an Org-unit is a structural unit."
	srv := initWithInstructionsServer(t, prose)
	c := NewClient(config.ServerConfig{Server: "timly", URL: srv.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	if got := c.Instructions(); got != prose {
		t.Errorf("Instructions = %q, want %q", got, prose)
	}
}

func TestClient_NoInstructionsField(t *testing.T) {
	srv := initWithInstructionsServer(t, "")
	c := NewClient(config.ServerConfig{Server: "noprose", URL: srv.URL + "/mcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()
	if got := c.Instructions(); got != "" {
		t.Errorf("Instructions = %q, want empty", got)
	}
}
