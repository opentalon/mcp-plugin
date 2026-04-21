package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opentalon/mcp-plugin/config"
)

// Client connects to one MCP server and exposes ListTools / CallTool.
// It auto-detects the transport: tries Streamable HTTP first, then falls
// back to the legacy HTTP+SSE transport.
type Client struct {
	cfg        config.ServerConfig
	tp         transport
	httpClient *http.Client
	idCounter  atomic.Int64
}

// NewClient creates a client for the given server config.
func NewClient(cfg config.ServerConfig) *Client {
	return &Client{cfg: cfg}
}

// Connect auto-detects the transport and performs the MCP initialize handshake.
func (c *Client) Connect(ctx context.Context) error {
	log.Printf("mcp-plugin: server %s: Connect begin url=%s", c.cfg.Server, c.cfg.URL)
	c.httpClient = &http.Client{
		Transport: &headerTransport{
			base:    http.DefaultTransport,
			headers: c.cfg.Headers,
		},
	}

	// Try Streamable HTTP first, with one retry on transient errors (EOF,
	// connection reset). Some wrappers like supergateway call res.end() twice,
	// which corrupts the HTTP response on the first attempt.
	var lastStreamableErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if err := c.tryStreamableHTTP(ctx); err == nil {
			log.Printf("mcp-plugin: server %s: Connect done (transport=StreamableHTTP)", c.cfg.Server)
			return nil
		} else {
			lastStreamableErr = err
			if attempt == 1 && isTransientHTTPErr(err) {
				log.Printf("mcp-plugin: server %s: Streamable HTTP transient error (%v), retrying once", c.cfg.Server, err)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			log.Printf("mcp-plugin: server %s: Streamable HTTP failed (%v), trying SSE", c.cfg.Server, err)
			break
		}
	}
	if err := c.connectSSE(ctx); err != nil {
		log.Printf("mcp-plugin: server %s: Connect failed: SSE: %v; StreamableHTTP: %v", c.cfg.Server, err, lastStreamableErr)
		return err
	}
	log.Printf("mcp-plugin: server %s: Connect done (transport=SSE)", c.cfg.Server)
	return nil
}

// tryStreamableHTTP attempts to connect using the Streamable HTTP transport.
func (c *Client) tryStreamableHTTP(ctx context.Context) error {
	log.Printf("mcp-plugin: server %s: initialize via StreamableHTTP (before roundTrip)", c.cfg.Server)
	st := newStreamableHTTP(ctx, c.httpClient, c.cfg.URL, c.cfg.Server)

	id := c.nextID()
	initReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: "2024-11-05",
			Capabilities:    map[string]interface{}{},
			ClientInfo:      clientInfo{Name: "opentalon-mcp", Version: "1.0"},
		},
	}

	resp, err := st.roundTrip(initReq, 15*time.Second, nil)
	if err != nil {
		st.close()
		return err
	}
	if resp.Error != nil {
		st.close()
		return fmt.Errorf("server %s: initialize: %s", c.cfg.Server, resp.Error.Message)
	}

	// Send notifications/initialized (fire-and-forget).
	notif := rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	_ = st.notify(notif)

	c.tp = st
	log.Printf("mcp-plugin: server %s: connected via Streamable HTTP (after initialize + notifications/initialized)", c.cfg.Server)
	return nil
}

// connectSSE connects using the legacy HTTP+SSE transport.
func (c *Client) connectSSE(ctx context.Context) error {
	log.Printf("mcp-plugin: server %s: connectSSE begin", c.cfg.Server)
	sse := newSSEConn(ctx)

	endpoint, err := sse.connect(c.httpClient, c.cfg.URL)
	if err != nil {
		return fmt.Errorf("server %s: SSE: %w", c.cfg.Server, err)
	}

	tp := &sseTransport{conn: sse, httpClient: c.httpClient, endpoint: endpoint, server: c.cfg.Server}

	// MCP initialize.
	log.Printf("mcp-plugin: server %s: SSE initialize (before roundTrip)", c.cfg.Server)
	id := c.nextID()
	initReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: "2024-11-05",
			Capabilities:    map[string]interface{}{},
			ClientInfo:      clientInfo{Name: "opentalon-mcp", Version: "1.0"},
		},
	}

	resp, err := tp.roundTrip(initReq, 15*time.Second, nil)
	if err != nil {
		tp.close()
		return fmt.Errorf("server %s: initialize: %w", c.cfg.Server, err)
	}
	if resp.Error != nil {
		tp.close()
		return fmt.Errorf("server %s: initialize: %s", c.cfg.Server, resp.Error.Message)
	}

	// Send notifications/initialized (fire-and-forget).
	notif := rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	_ = tp.notify(notif)

	c.tp = tp
	log.Printf("mcp-plugin: server %s: connected via SSE (after initialize + notifications/initialized)", c.cfg.Server)
	return nil
}

// ListTools calls tools/list and returns the server's tool list.
func (c *Client) ListTools() ([]Tool, error) {
	id := c.nextID()
	log.Printf("mcp-plugin: server %s: ListTools begin jsonrpc_id=%d", c.cfg.Server, id)
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/list",
		Params:  map[string]interface{}{},
	}

	resp, err := c.tp.roundTrip(req, 30*time.Second, nil)
	if err != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d err: %v", c.cfg.Server, id, err)
		return nil, err
	}
	if resp.Error != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d rpc error: %s", c.cfg.Server, id, resp.Error.Message)
		return nil, fmt.Errorf("tools/list: %s", resp.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d decode err: %v", c.cfg.Server, id, err)
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	log.Printf("mcp-plugin: server %s: ListTools ok jsonrpc_id=%d count=%d", c.cfg.Server, id, len(result.Tools))
	return result.Tools, nil
}

// CallTool invokes an MCP tool and returns the text content of the response.
// extraHeaders are per-request credential headers (from WhoAmI) that override
// the client's static config headers. Pass nil when not needed.
func (c *Client) CallTool(name string, args map[string]interface{}, extraHeaders http.Header) (string, error) {
	id := c.nextID()
	argKeys := make([]string, 0, len(args))
	for k := range args {
		argKeys = append(argKeys, k)
	}
	sort.Strings(argKeys)
	log.Printf("mcp-plugin: server %s: CallTool begin tool=%q jsonrpc_id=%d arg_count=%d arg_keys=%v",
		c.cfg.Server, name, id, len(args), argKeys)

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/call",
		Params: toolsCallParams{
			Name:      name,
			Arguments: args,
		},
	}

	resp, err := c.tp.roundTrip(req, 60*time.Second, extraHeaders)
	if err != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d err: %v", c.cfg.Server, name, id, err)
		return "", err
	}
	if resp.Error != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d rpc error: %s", c.cfg.Server, name, id, resp.Error.Message)
		return "", fmt.Errorf("tools/call %s: %s", name, resp.Error.Message)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d decode err: %v", c.cfg.Server, name, id, err)
		return "", fmt.Errorf("decode tools/call: %w", err)
	}
	if result.IsError {
		var parts []string
		for _, co := range result.Content {
			if co.Text != "" {
				parts = append(parts, co.Text)
			}
		}
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d tool error in body", c.cfg.Server, name, id)
		return "", fmt.Errorf("tool %s error: %s", name, strings.Join(parts, "; "))
	}
	var parts []string
	for _, item := range result.Content {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	out := strings.Join(parts, "\n")
	log.Printf("mcp-plugin: server %s: CallTool ok tool=%q jsonrpc_id=%d content_len=%d", c.cfg.Server, name, id, len(out))
	return out, nil
}

// IsStreamableHTTP reports whether the client is using the Streamable HTTP transport.
func (c *Client) IsStreamableHTTP() bool {
	_, ok := c.tp.(*streamableHTTP)
	return ok
}

// FallbackSSE closes the current transport and re-connects using SSE.
// This is useful when the server accepts a StreamableHTTP initialize but
// fails on subsequent calls (e.g. tools/list returns 503) because it only
// truly supports the SSE transport.
func (c *Client) FallbackSSE(ctx context.Context) error {
	log.Printf("mcp-plugin: server %s: StreamableHTTP→SSE fallback (closing old transport)", c.cfg.Server)
	c.tp.close()
	c.tp = nil
	c.idCounter.Store(0)
	if err := c.connectSSE(ctx); err != nil {
		return err
	}
	log.Printf("mcp-plugin: server %s: Connect done (transport=SSE, fallback)", c.cfg.Server)
	return nil
}

// Close shuts down the transport (cancels SSE readLoop, etc.).
// Safe to call on a nil or not-yet-connected client.
func (c *Client) Close() {
	if c.tp != nil {
		c.tp.close()
	}
}

// ServerName returns the configured server name.
func (c *Client) ServerName() string { return c.cfg.Server }

// IsAlive reports whether the client has an active transport connection.
func (c *Client) IsAlive() bool {
	return c.tp != nil && c.tp.context().Err() == nil
}

// TransportContextErr returns the transport context error if the transport is
// dead, or nil if it is alive (or not yet initialized).
func (c *Client) TransportContextErr() error {
	if c.tp == nil {
		return nil
	}
	return c.tp.context().Err()
}

func (c *Client) nextID() int64 {
	return c.idCounter.Add(1)
}

// isTransientHTTPErr returns true for errors that are likely caused by the
// server closing the connection prematurely (EOF, connection reset) and may
// succeed on a retry.
func isTransientHTTPErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe")
}

// resolveEndpoint resolves endpoint (possibly relative) against sseURL.
func resolveEndpoint(sseURL, endpoint string) string {
	base, err := url.Parse(sseURL)
	if err != nil {
		return endpoint
	}
	ep, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return base.ResolveReference(ep).String()
}

// extraHeadersKey is a context key for per-request credential headers.
type extraHeadersKey struct{}

// contextWithExtraHeaders returns a child context carrying per-request HTTP
// headers. headerTransport applies these after static config headers so that
// credential headers from WhoAmI take priority.
func contextWithExtraHeaders(ctx context.Context, h http.Header) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, extraHeadersKey{}, h)
}

// headerTransport injects fixed headers into every outbound request.
// Per-request credential headers (carried via context) override static ones.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	// Per-request credential headers override static config headers.
	if extra, ok := clone.Context().Value(extraHeadersKey{}).(http.Header); ok {
		for k, vals := range extra {
			for _, v := range vals {
				clone.Header.Set(k, v)
			}
		}
	}
	return t.base.RoundTrip(clone)
}
