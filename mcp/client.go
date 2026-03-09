package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opentalon/mcp-plugin/config"
)

// Client connects to one MCP server over HTTP+SSE and exposes ListTools / CallTool.
type Client struct {
	cfg        config.ServerConfig
	sse        *sseConn
	httpClient *http.Client
	idCounter  atomic.Int64
}

// NewClient creates a client for the given server config.
func NewClient(cfg config.ServerConfig) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			// No timeout on the transport level — SSE is long-lived.
			// Individual RPC calls set their own deadlines.
		},
	}
}

// Connect opens the SSE stream and performs the MCP initialize handshake.
func (c *Client) Connect(ctx context.Context) error {
	c.sse = newSSEConn(ctx)

	// Build HTTP client with per-request headers via RoundTripper wrapper.
	c.httpClient = &http.Client{
		Transport: &headerTransport{
			base:    http.DefaultTransport,
			headers: c.cfg.Headers,
		},
	}

	// Open SSE connection.
	sseURL := c.cfg.URL
	endpoint, err := c.sse.connect(c.httpClient, sseURL)
	if err != nil {
		return fmt.Errorf("server %s: SSE: %w", c.cfg.Server, err)
	}
	c.sse.endpoint = endpoint

	// MCP initialize.
	id := c.nextID()
	ch := c.sse.subscribe(id)
	defer c.sse.unsubscribe(id)

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
	if err := c.post(initReq); err != nil {
		return fmt.Errorf("server %s: initialize: %w", c.cfg.Server, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("server %s: initialize: %s", c.cfg.Server, resp.Error.Message)
		}
	case <-time.After(15 * time.Second):
		return fmt.Errorf("server %s: initialize timeout", c.cfg.Server)
	case <-c.sse.ctx.Done():
		return fmt.Errorf("server %s: SSE closed during initialize", c.cfg.Server)
	}

	// Send notifications/initialized (fire-and-forget notification).
	notif := rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		// No ID — it's a notification, not a request.
	}
	_ = c.post(notif) // best-effort

	return nil
}

// ListTools calls tools/list and returns the server's tool list.
func (c *Client) ListTools() ([]Tool, error) {
	id := c.nextID()
	ch := c.sse.subscribe(id)
	defer c.sse.unsubscribe(id)

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/list",
		Params:  map[string]interface{}{},
	}
	if err := c.post(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list: %s", resp.Error.Message)
		}
		var result toolsListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("decode tools/list: %w", err)
		}
		return result.Tools, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("tools/list timeout")
	case <-c.sse.ctx.Done():
		return nil, fmt.Errorf("SSE closed during tools/list")
	}
}

// CallTool invokes an MCP tool and returns the text content of the response.
func (c *Client) CallTool(name string, args map[string]interface{}) (string, error) {
	id := c.nextID()
	ch := c.sse.subscribe(id)
	defer c.sse.unsubscribe(id)

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/call",
		Params: toolsCallParams{
			Name:      name,
			Arguments: args,
		},
	}
	if err := c.post(req); err != nil {
		return "", err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return "", fmt.Errorf("tools/call %s: %s", name, resp.Error.Message)
		}
		var result toolsCallResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return "", fmt.Errorf("decode tools/call: %w", err)
		}
		if result.IsError {
			// Collect error text from content items.
			var parts []string
			for _, c := range result.Content {
				if c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
			return "", fmt.Errorf("tool %s error: %s", name, strings.Join(parts, "; "))
		}
		var parts []string
		for _, item := range result.Content {
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, "\n"), nil
	case <-time.After(60 * time.Second):
		return "", fmt.Errorf("tools/call %s: timeout", name)
	case <-c.sse.ctx.Done():
		return "", fmt.Errorf("SSE closed during tools/call %s", name)
	}
}

// ServerName returns the configured server name.
func (c *Client) ServerName() string { return c.cfg.Server }

// post encodes req as JSON and POSTs it to the session endpoint.
func (c *Client) post(req rpcRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(c.sse.ctx, http.MethodPost, c.sse.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) nextID() int64 {
	return c.idCounter.Add(1)
}

// jsonUnmarshal is a package-local helper used by sse.go.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
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

// headerTransport injects fixed headers into every outbound request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the original.
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}
