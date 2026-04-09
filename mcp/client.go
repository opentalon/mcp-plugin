package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
	c.httpClient = &http.Client{
		Transport: &headerTransport{
			base:    http.DefaultTransport,
			headers: c.cfg.Headers,
		},
	}

	// Try Streamable HTTP first.
	if err := c.tryStreamableHTTP(ctx); err == nil {
		return nil
	} else {
		log.Printf("mcp-plugin: server %s: Streamable HTTP failed (%v), trying SSE", c.cfg.Server, err)
	}
	return c.connectSSE(ctx)
}

// tryStreamableHTTP attempts to connect using the Streamable HTTP transport.
func (c *Client) tryStreamableHTTP(ctx context.Context) error {
	st := newStreamableHTTP(ctx, c.httpClient, c.cfg.URL)

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

	resp, err := st.roundTrip(initReq, 15*time.Second)
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
	log.Printf("mcp-plugin: server %s: connected via Streamable HTTP", c.cfg.Server)
	return nil
}

// connectSSE connects using the legacy HTTP+SSE transport.
func (c *Client) connectSSE(ctx context.Context) error {
	sse := newSSEConn(ctx)

	endpoint, err := sse.connect(c.httpClient, c.cfg.URL)
	if err != nil {
		return fmt.Errorf("server %s: SSE: %w", c.cfg.Server, err)
	}

	tp := &sseTransport{conn: sse, httpClient: c.httpClient, endpoint: endpoint}

	// MCP initialize.
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

	resp, err := tp.roundTrip(initReq, 15*time.Second)
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
	log.Printf("mcp-plugin: server %s: connected via SSE", c.cfg.Server)
	return nil
}

// ListTools calls tools/list and returns the server's tool list.
func (c *Client) ListTools() ([]Tool, error) {
	id := c.nextID()
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/list",
		Params:  map[string]interface{}{},
	}

	resp, err := c.tp.roundTrip(req, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list: %s", resp.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	return result.Tools, nil
}

// CallTool invokes an MCP tool and returns the text content of the response.
func (c *Client) CallTool(name string, args map[string]interface{}) (string, error) {
	id := c.nextID()
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/call",
		Params: toolsCallParams{
			Name:      name,
			Arguments: args,
		},
	}

	resp, err := c.tp.roundTrip(req, 60*time.Second)
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("tools/call %s: %s", name, resp.Error.Message)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("decode tools/call: %w", err)
	}
	if result.IsError {
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
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}
