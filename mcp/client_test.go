package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
	got, err := c.CallTool("echo", map[string]interface{}{"text": "hello world"})
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
	_, err := c.CallTool("unknown-tool", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
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
	got, err := c.CallTool("echo", map[string]interface{}{"text": "streamable hello"})
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
	_, err := c.CallTool("unknown-tool", map[string]interface{}{})
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
