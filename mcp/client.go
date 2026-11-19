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

// protocolVersion is the MCP protocol revision advertised on initialize. We
// negotiate against `2025-03-26` (the first revision that allows servers to
// return `instructions`); on `2024-11-05` the spec — and the Ruby `mcp` gem
// at server side — explicitly strip `instructions` from the initialize
// response, leaving downstream RAG / system-prompt enrichment empty.
const protocolVersion = "2025-03-26"

// toolCallTimeout bounds a tools/call round trip, and a check's.
const toolCallTimeout = 60 * time.Second

// Client connects to one MCP server and exposes ListTools / CallTool.
// It auto-detects the transport: tries Streamable HTTP first, then falls
// back to the legacy HTTP+SSE transport.
type Client struct {
	cfg               config.ServerConfig
	tp                transport
	httpClient        *http.Client
	idCounter         atomic.Int64
	instructions      string                 // server-provided initialize.instructions text (may be empty)
	glossary          []InitGlossaryEntry    // server-provided glossary entries (may be empty)
	knowledgeArticles []InitKnowledgeArticle // server-provided per-section knowledge articles (may be empty)
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

	resp, err := c.sendInitialize(st)
	if err != nil {
		st.close()
		return err
	}
	if err := c.finishInitialize(st, resp); err != nil {
		st.close()
		return err
	}
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
	resp, err := c.sendInitialize(tp)
	if err != nil {
		tp.close()
		return fmt.Errorf("server %s: initialize: %w", c.cfg.Server, err)
	}
	if err := c.finishInitialize(tp, resp); err != nil {
		tp.close()
		return err
	}
	log.Printf("mcp-plugin: server %s: connected via SSE (after initialize + notifications/initialized)", c.cfg.Server)
	return nil
}

// sendInitialize sends the MCP initialize request over tp and returns the
// round-trip error as is. The caller decides how to report it: the Streamable
// HTTP path returns it unchanged, the SSE path prefixes the server name.
func (c *Client) sendInitialize(tp transport) (rpcResponse, error) {
	id := c.nextID()
	initReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]interface{}{},
			ClientInfo:      clientInfo{Name: "opentalon-mcp", Version: "1.0"},
		},
	}
	return tp.roundTrip(context.Background(), initReq, 15*time.Second, nil)
}

// finishInitialize completes the handshake on an initialize response: it fails
// on an error the server reported, otherwise keeps the server's instructions,
// glossary and knowledge articles, sends notifications/initialized
// (fire-and-forget) and makes tp the client's transport. The caller closes tp
// on error.
func (c *Client) finishInitialize(tp transport, resp rpcResponse) error {
	if resp.Error != nil {
		return fmt.Errorf("server %s: initialize: %s", c.cfg.Server, resp.Error)
	}
	c.instructions, c.glossary, c.knowledgeArticles = decodeInitResult(c.cfg.Server, resp.Result)
	_ = tp.notify(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	c.tp = tp
	return nil
}

// Instructions returns the server's `initialize.instructions` text, if any.
// Empty string when the server didn't provide instructions or hasn't connected.
func (c *Client) Instructions() string { return c.instructions }

// Glossary returns the server's glossary entries from the initialize response.
// Empty slice when the server didn't provide a glossary or hasn't connected.
func (c *Client) Glossary() []InitGlossaryEntry { return c.glossary }

// KnowledgeArticles returns the server's per-section knowledge articles from
// the initialize response. Empty slice when the server didn't provide any or
// hasn't connected.
func (c *Client) KnowledgeArticles() []InitKnowledgeArticle { return c.knowledgeArticles }

// decodeInitResult extracts `instructions`, `glossary` and `knowledge_articles`
// from an MCP initialize result payload. All are optional.
func decodeInitResult(server string, raw json.RawMessage) (string, []InitGlossaryEntry, []InitKnowledgeArticle) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var r initializeResult
	if err := json.Unmarshal(raw, &r); err != nil {
		log.Printf("mcp-plugin: server %s: initialize: decode: %v", server, err)
		return "", nil, nil
	}
	if r.Instructions != "" {
		log.Printf("mcp-plugin: server %s: initialize: captured %d bytes of instructions", server, len(r.Instructions))
	}
	if len(r.Glossary) > 0 {
		log.Printf("mcp-plugin: server %s: initialize: captured %d glossary entries", server, len(r.Glossary))
	}
	if len(r.KnowledgeArticles) > 0 {
		log.Printf("mcp-plugin: server %s: initialize: captured %d knowledge articles", server, len(r.KnowledgeArticles))
	}
	return r.Instructions, r.Glossary, r.KnowledgeArticles
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

	resp, err := c.tp.roundTrip(context.Background(), req, 30*time.Second, nil)
	if err != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d err: %v", c.cfg.Server, id, err)
		return nil, err
	}
	if resp.Error != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d rpc error: %s", c.cfg.Server, id, resp.Error)
		return nil, fmt.Errorf("tools/list: %s", resp.Error)
	}
	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		log.Printf("mcp-plugin: server %s: ListTools jsonrpc_id=%d decode err: %v", c.cfg.Server, id, err)
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	log.Printf("mcp-plugin: server %s: ListTools ok jsonrpc_id=%d count=%d", c.cfg.Server, id, len(result.Tools))
	return result.Tools, nil
}

// CallTool invokes an MCP tool and returns the text content and optional
// structured content (MCP revision 2025-06+) of the response.
// extraHeaders are per-request credential headers (from WhoAmI) that override
// the client's static config headers. Pass nil when not needed.
func (c *Client) CallTool(name string, args map[string]interface{}, extraHeaders http.Header) (content, structured string, err error) {
	id := c.nextID()
	argKeys := make([]string, 0, len(args))
	for k := range args {
		argKeys = append(argKeys, k)
	}
	sort.Strings(argKeys)
	argDetails := make([]string, 0, len(args))
	for _, k := range argKeys {
		argDetails = append(argDetails, fmt.Sprintf("%s=%v(%T)", k, args[k], args[k]))
	}
	log.Printf("mcp-plugin: server %s: CallTool begin tool=%q jsonrpc_id=%d arg_count=%d args=[%s]",
		c.cfg.Server, name, id, len(args), strings.Join(argDetails, ", "))

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "tools/call",
		Params: toolsCallParams{
			Name:      name,
			Arguments: args,
		},
	}

	if reqBody, jsonErr := json.Marshal(req); jsonErr == nil {
		log.Printf("mcp-plugin: server %s: CallTool jsonrpc_id=%d request_body=%s", c.cfg.Server, id, string(reqBody))
	}

	resp, err := c.tp.roundTrip(context.Background(), req, toolCallTimeout, extraHeaders)
	if err != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d err: %v", c.cfg.Server, name, id, err)
		return "", "", err
	}
	if resp.Error != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d rpc error: %s", c.cfg.Server, name, id, resp.Error)
		return "", "", fmt.Errorf("tools/call %s: %s", name, resp.Error)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d decode err: %v", c.cfg.Server, name, id, err)
		return "", "", fmt.Errorf("decode tools/call: %w", err)
	}
	if result.IsError {
		var parts []string
		for _, co := range result.Content {
			if co.Text != "" {
				parts = append(parts, co.Text)
			}
		}
		log.Printf("mcp-plugin: server %s: CallTool tool=%q jsonrpc_id=%d tool error in body", c.cfg.Server, name, id)
		return "", "", errors.New(toolErrorText(name, strings.Join(parts, "; ")))
	}
	var parts []string
	for _, item := range result.Content {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	content = strings.Join(parts, "\n")
	structured = structuredPayload(result.StructuredContent)
	log.Printf("mcp-plugin: server %s: CallTool ok tool=%q jsonrpc_id=%d content_len=%d structured_len=%d", c.cfg.Server, name, id, len(content), len(structured))
	return content, structured, nil
}

// toolErrorText is the text a tool's refusal reaches the host with: the tool's
// own text behind a lead-in that names the tool.
func toolErrorText(name, text string) string {
	return fmt.Sprintf("tool %s error: %s", name, text)
}

// checkMethod is the method on which an MCP server checks a tool call without
// executing it. It is OpenTalon's, not MCP's: a server without it answers
// "method not found".
const checkMethod = "opentalon/tools/check"

// CheckResult is an MCP server's verdict on a tool call sent for a check.
type CheckResult struct {
	// Refused is false when the call passed the check.
	Refused bool
	// Kind is the kind of error the execution would give, such as
	// "forbidden" or "invalid_argument".
	Kind string
	// Message is the text an execution's refusal of the call reaches the
	// host with, so the model reads the same refusal either way.
	Message string
}

// CheckTool sends a tool call to the server's checkMethod, with the
// parameters CallTool sends, and returns the server's verdict; the server
// does not execute the call. The round trip ends with ctx, and no later than
// a tool call's.
//
// Any answer that is no verdict is an error: an error of the method (the
// server does not know it, the tool has no check or is unknown, the server
// failed), a transport error, the end of ctx, a result that cannot be read, a
// verdict this client does not know, and a refusal without a message, which
// the host takes as no verdict and the lead-in would otherwise hide.
func (c *Client) CheckTool(ctx context.Context, name string, args map[string]interface{}, extraHeaders http.Header) (CheckResult, error) {
	id := c.nextID()
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  checkMethod,
		Params: toolsCallParams{
			Name:      name,
			Arguments: args,
		},
	}
	resp, err := c.tp.roundTrip(ctx, req, toolCallTimeout, extraHeaders)
	if err != nil {
		return CheckResult{}, err
	}
	if resp.Error != nil {
		return CheckResult{}, fmt.Errorf("%s %s: %s", checkMethod, name, resp.Error)
	}
	var answer checkAnswer
	if err := json.Unmarshal(resp.Result, &answer); err != nil {
		return CheckResult{}, fmt.Errorf("decode %s: %w", checkMethod, err)
	}
	switch {
	case answer.Verdict == "passed":
		return CheckResult{}, nil
	case answer.Verdict != "refused":
		return CheckResult{}, fmt.Errorf("%s %s: no verdict in %s", checkMethod, name, resp.Result)
	case strings.TrimSpace(answer.Message) == "":
		return CheckResult{}, fmt.Errorf("%s %s: refused without a message", checkMethod, name)
	}
	return CheckResult{Refused: true, Kind: answer.Kind, Message: toolErrorText(name, answer.Message)}, nil
}

// structuredPayload normalises a raw JSON structured-content payload:
// nil, empty, and JSON-null are mapped to "" so callers can use a simple
// emptiness check.
func structuredPayload(sc json.RawMessage) string {
	if len(sc) == 0 {
		return ""
	}
	if string(sc) == "null" {
		return ""
	}
	return string(sc)
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
// Safe to call on a client that is not connected; the client itself must not
// be nil.
func (c *Client) Close() {
	if c.tp != nil {
		c.tp.close()
	}
}

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
