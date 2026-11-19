package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Over the SSE transport a check ends with its context, whether the server
// holds the POST open or accepts it and never answers on the stream.
func TestSSE_CheckTool_endsAtTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		holdPost bool
	}{
		{"POST held open", true},
		{"no answer on the stream", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeMCPServer{eventsCh: make(chan string, 32)}
			messages := fs.messageHandler(t)
			mux := http.NewServeMux()
			mux.HandleFunc("/sse", fs.sseHandler)
			mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req rpcRequest
				_ = json.Unmarshal(body, &req)
				if req.Method != checkMethod {
					r.Body = io.NopCloser(bytes.NewReader(body))
					messages(w, r)
					return
				}
				if tc.holdPost {
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
					return
				}
				w.WriteHeader(http.StatusAccepted)
			})
			fs.srv = httptest.NewServer(mux)
			t.Cleanup(fs.srv.Close)

			c := NewClient(fs.cfg())
			if err := c.Connect(testCtx(t)); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if c.IsStreamableHTTP() {
				t.Fatal("connected over Streamable HTTP, want SSE")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			if _, err := c.CheckTool(ctx, "echo", map[string]interface{}{"text": "hi"}, nil); err == nil {
				t.Fatal("CheckTool returned a verdict, want an error")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("CheckTool took %v, want it to end at the 200ms deadline", elapsed)
			}
		})
	}
}
