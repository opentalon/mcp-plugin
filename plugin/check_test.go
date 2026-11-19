package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/mcp-plugin/config"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// checkReply is how checkServer answers opentalon/tools/check.
type checkReply struct {
	status  int    // HTTP status other than 200, no JSON-RPC answer
	members string // JSON-RPC members after the id, e.g. `"result":{"verdict":"passed"}`
	hold    bool   // no answer until the client gives up
}

// sentRequest is one tools/call or check request as checkServer received it.
type sentRequest struct {
	params  map[string]interface{}
	headers http.Header
}

// checkServer is a Streamable HTTP MCP server with one tool, set_value, whose
// schema makes the preparation of a call change its arguments. It answers
// tools/call with a tool error reading refusal and a check with reply, and
// records both kinds of request.
type checkServer struct {
	srv   *httptest.Server
	reply checkReply

	mu   sync.Mutex
	sent map[string][]sentRequest // by JSON-RPC method
}

const refusal = "The name Drill is taken."

func newCheckServer(t *testing.T, reply checkReply) *checkServer {
	t.Helper()
	fs := &checkServer{reply: reply, sent: map[string][]sentRequest{}}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64                 `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var members string
		switch req.Method {
		case "initialize":
			members = `"result":{"protocolVersion":"2025-03-26","capabilities":{}}`
		case "tools/list":
			members = `"result":{"tools":[{"name":"set_value","description":"x","inputSchema":{"type":"object","properties":{
				"count":{"type":"integer"},
				"tags":{"type":"array","items":{"type":"string"}},
				"include_fields":{"type":"array","description":"Extra fields, a subset of: notes, owner."},
				"tool_name":{"type":"string"}
			}},"_meta":{"check":true}}]}`
		case "tools/call", "opentalon/tools/check":
			fs.mu.Lock()
			fs.sent[req.Method] = append(fs.sent[req.Method], sentRequest{params: req.Params, headers: r.Header.Clone()})
			fs.mu.Unlock()
			if req.Method == "tools/call" {
				members = fmt.Sprintf(`"result":{"content":[{"type":"text","text":%q}],"isError":true}`, refusal)
				break
			}
			switch {
			case fs.reply.hold:
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			case fs.reply.status != 0:
				w.WriteHeader(fs.reply.status)
				return
			}
			members = fs.reply.members
		default:
			members = `"result":{}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,%s}`, *req.ID, members)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *checkServer) requests(method string) []sentRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.sent[method]
}

// handler builds a Handler on fs with a static header and one context header.
func (fs *checkServer) handler(t *testing.T) *Handler {
	t.Helper()
	ctx := testCtx(t)
	cfg := config.ServerConfig{
		Server:         "srv",
		URL:            fs.srv.URL + "/mcp",
		Headers:        map[string]string{"X-Static": "from-config"},
		ContextHeaders: map[string]string{"session_id": "X-Session-Id"},
	}
	h := NewHandler(ctx)
	h.SetRegistry(Build(ctx, []config.ServerConfig{cfg}), nil)
	return h
}

// checkRequest is a call whose every argument the preparation changes.
var checkRequest = pluginpkg.Request{
	ID:     "call-1",
	Action: "srv__set_value",
	Args: map[string]string{
		"count":          "7",
		"tags":           "red",
		"include_fields": `["name","notes"]`,
		"tool_name":      "srv__set_value",
		"session_id":     "sess-1",
	},
	CredentialHeaders: map[string]pluginpkg.CredentialHeader{"srv": {Header: "X-Timly-User-Id", Value: "42"}},
}

func TestHandler_Check_sendsWhatExecuteSends(t *testing.T) {
	fs := newCheckServer(t, checkReply{members: `"result":{"verdict":"passed"}`})
	h := fs.handler(t)

	h.Execute(checkRequest)
	if got := h.Check(context.Background(), checkRequest); got.Verdict != pluginpkg.CheckPassed {
		t.Fatalf("Check verdict = %v, want passed", got.Verdict)
	}
	calls, checks := fs.requests("tools/call"), fs.requests("opentalon/tools/check")
	if len(calls) != 1 || len(checks) != 1 {
		t.Fatalf("got %d tools/call and %d checks, want one of each", len(calls), len(checks))
	}
	call, check := calls[0], checks[0]

	var want map[string]interface{}
	_ = json.Unmarshal([]byte(`{"name":"set_value","arguments":{"count":7,"tags":["red"],"include_fields":["notes"],"tool_name":"set_value"}}`), &want)
	if !reflect.DeepEqual(call.params, want) {
		t.Errorf("tools/call params = %v, want the prepared call %v", call.params, want)
	}
	if !reflect.DeepEqual(check.params, call.params) {
		t.Errorf("check params = %v, want what tools/call got: %v", check.params, call.params)
	}

	for _, name := range []string{"X-Session-Id", "X-Timly-User-Id", "X-Static"} {
		if call.headers.Get(name) == "" {
			t.Errorf("tools/call carries no %s header", name)
		}
	}
	// The bodies differ in the method name, and with it in length.
	call.headers.Del("Content-Length")
	check.headers.Del("Content-Length")
	if !reflect.DeepEqual(check.headers, call.headers) {
		t.Errorf("check headers = %v, want what tools/call got: %v", check.headers, call.headers)
	}
}

func TestHandler_Check_verdict(t *testing.T) {
	cases := []struct {
		name     string
		reply    checkReply
		deadline time.Duration
		want     pluginpkg.CheckVerdict
		wantKind string
	}{
		{name: "passed", reply: checkReply{members: `"result":{"verdict":"passed"}`}, want: pluginpkg.CheckPassed},
		{
			name:  "refused",
			reply: checkReply{members: fmt.Sprintf(`"result":{"verdict":"refused","kind":"invalid_argument","message":%q}`, refusal)},
			want:  pluginpkg.CheckRefused, wantKind: "invalid_argument",
		},
		{name: "refused without a message", reply: checkReply{members: `"result":{"verdict":"refused","kind":"invalid_argument","message":" "}`}},
		{name: "method not found", reply: checkReply{members: `"error":{"code":-32601,"message":"Method not found"}`}},
		{name: "tool without a check", reply: checkReply{members: `"error":{"code":-32602,"message":"Invalid params","data":"set_value has no check"}`}},
		{name: "empty result", reply: checkReply{members: `"result":{}`}},
		{name: "unknown verdict", reply: checkReply{members: `"result":{"verdict":"maybe"}`}},
		{name: "result not an object", reply: checkReply{members: `"result":"passed"`}},
		{name: "transport error", reply: checkReply{status: http.StatusInternalServerError}},
		{name: "deadline", reply: checkReply{hold: true}, deadline: 200 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newCheckServer(t, tc.reply)
			h := fs.handler(t)
			ctx := context.Background()
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}

			start := time.Now()
			got := h.Check(ctx, checkRequest)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("Check took %v", elapsed)
			}
			if n := len(fs.requests("opentalon/tools/check")); n != 1 {
				t.Errorf("server got %d checks, want 1", n)
			}
			if n := len(fs.requests("tools/call")); n != 0 {
				t.Fatalf("a check sent %d tools/call", n)
			}
			if got.Verdict != tc.want || got.Kind != tc.wantKind {
				t.Fatalf("Check = %+v, want verdict %v kind %q", got, tc.want, tc.wantKind)
			}
			if tc.want != pluginpkg.CheckRefused {
				if got.Message != "" {
					t.Errorf("Message = %q, want none", got.Message)
				}
				return
			}
			if want := "tool set_value error: " + refusal; got.Message != want {
				t.Errorf("Message = %q, want %q", got.Message, want)
			}
			if exec := h.Execute(checkRequest); got.Message != exec.Error {
				t.Errorf("Message = %q, want the error the execution's refusal has: %q", got.Message, exec.Error)
			}
		})
	}
}
