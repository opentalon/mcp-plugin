package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseTransport implements the transport interface over HTTP+SSE.
type sseTransport struct {
	conn       *sseConn
	httpClient *http.Client
	endpoint   string
	server     string // config server key; only for logs
}

func (s *sseTransport) roundTrip(req rpcRequest, timeout time.Duration, extraHeaders http.Header) (rpcResponse, error) {
	if req.ID == nil {
		return rpcResponse{}, fmt.Errorf("roundTrip requires a request ID")
	}
	id := *req.ID
	log.Printf("mcp-plugin: server %s: SSE → %s jsonrpc_id=%d timeout=%v (subscribe + before POST)", s.server, req.Method, id, timeout)

	ch := s.conn.subscribe(id)
	defer s.conn.unsubscribe(id)

	if err := s.post(req, extraHeaders); err != nil {
		log.Printf("mcp-plugin: server %s: SSE ← %s jsonrpc_id=%d POST failed: %v", s.server, req.Method, id, err)
		return rpcResponse{}, err
	}
	log.Printf("mcp-plugin: server %s: SSE … %s jsonrpc_id=%d POST accepted, waiting for SSE event", s.server, req.Method, id)

	select {
	case resp := <-ch:
		log.Printf("mcp-plugin: server %s: SSE ← %s jsonrpc_id=%d ok (response received)", s.server, req.Method, id)
		return resp, nil
	case <-time.After(timeout):
		log.Printf("mcp-plugin: server %s: SSE ← %s jsonrpc_id=%d timeout after %v", s.server, req.Method, id, timeout)
		return rpcResponse{}, fmt.Errorf("%s: timeout", req.Method)
	case <-s.conn.ctx.Done():
		log.Printf("mcp-plugin: server %s: SSE ← %s jsonrpc_id=%d transport closed: %v", s.server, req.Method, id, s.conn.ctx.Err())
		return rpcResponse{}, fmt.Errorf("SSE closed during %s", req.Method)
	}
}

func (s *sseTransport) notify(req rpcRequest) error {
	log.Printf("mcp-plugin: server %s: SSE → %s (notify, before POST)", s.server, req.Method)
	err := s.post(req, nil)
	if err != nil {
		log.Printf("mcp-plugin: server %s: SSE ← notify %s err: %v", s.server, req.Method, err)
		return err
	}
	log.Printf("mcp-plugin: server %s: SSE ← notify %s ok", s.server, req.Method)
	return nil
}

func (s *sseTransport) post(req rpcRequest, extraHeaders http.Header) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	ctx := contextWithExtraHeaders(s.conn.ctx, extraHeaders)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *sseTransport) context() context.Context { return s.conn.ctx }
func (s *sseTransport) close()                    { s.conn.cancel() }

// sseConn manages a persistent SSE connection to an MCP server.
// It reads events from the stream and dispatches JSON-RPC responses
// to waiting callers keyed by request ID.
type sseConn struct {
	mu      sync.Mutex
	pending map[int64]chan rpcResponse

	endpointCh chan string // closed after endpoint is received once
	endpointOnce sync.Once
	endpoint   string

	ctx    context.Context
	cancel context.CancelFunc
}

func newSSEConn(ctx context.Context) *sseConn {
	c, cancel := context.WithCancel(ctx)
	return &sseConn{
		pending:    make(map[int64]chan rpcResponse),
		endpointCh: make(chan string, 1),
		ctx:        c,
		cancel:     cancel,
	}
}

// subscribe registers a channel to receive the response with the given ID.
func (s *sseConn) subscribe(id int64) chan rpcResponse {
	ch := make(chan rpcResponse, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	return ch
}

// unsubscribe removes the pending entry for id.
func (s *sseConn) unsubscribe(id int64) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// dispatch delivers a response to the waiting subscriber, if any.
func (s *sseConn) dispatch(resp rpcResponse) {
	var id int64
	switch v := resp.ID.(type) {
	case float64:
		id = int64(v)
	case int64:
		id = v
	default:
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[id]
	s.mu.Unlock()
	if ok {
		ch <- resp
	} else {
		log.Printf("mcp-plugin: SSE dispatch: jsonrpc_id=%d has no subscriber (response dropped; likely arrived before subscribe or wrong id)", id)
	}
}

// connect opens the SSE stream to sseURL and starts reading events in a
// goroutine. It returns the per-session POST endpoint URL.
func (s *sseConn) connect(httpClient *http.Client, sseURL string) (string, error) {
	log.Printf("mcp-plugin: SSE GET connect (before request) url=%s", sseURL)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return "", fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("mcp-plugin: SSE GET connect failed: %v", err)
		return "", fmt.Errorf("SSE connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		log.Printf("mcp-plugin: SSE GET connect HTTP %d", resp.StatusCode)
		return "", fmt.Errorf("SSE connect: HTTP %d", resp.StatusCode)
	}
	log.Printf("mcp-plugin: SSE GET connect HTTP 200, starting readLoop (waiting for endpoint event)")

	go s.readLoop(resp, sseURL)

	// Wait for the endpoint event (first event from server).
	select {
	case ep := <-s.endpointCh:
		log.Printf("mcp-plugin: SSE endpoint resolved POST url=%s", ep)
		return ep, nil
	case <-s.ctx.Done():
		log.Printf("mcp-plugin: SSE endpoint wait cancelled: %v", s.ctx.Err())
		return "", s.ctx.Err()
	}
}

// readLoop reads SSE events and dispatches them.
func (s *sseConn) readLoop(resp *http.Response, sseURL string) {
	defer func() { _ = resp.Body.Close() }()
	defer s.cancel()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), sseMaxTokenSize)
	var scanErr error
	defer func() {
		if s.ctx.Err() == nil { // not cancelled by us — unexpected drop
			if scanErr != nil {
				log.Printf("mcp-plugin: SSE stream %s disconnected: %v", sseURL, scanErr)
			} else {
				log.Printf("mcp-plugin: SSE stream %s disconnected (server closed connection)", sseURL)
			}
		}
	}()
	var eventType string
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Blank line: dispatch accumulated event.
			s.handleEvent(eventType, strings.Join(dataLines, "\n"), sseURL)
			eventType = ""
			dataLines = nil
			continue
		}

		if after, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(after))
		}
		// comments (": ...") and unknown fields are ignored per SSE spec
	}
	scanErr = scanner.Err()
}

func (s *sseConn) handleEvent(eventType, data, sseURL string) {
	switch eventType {
	case "endpoint":
		endpoint := resolveEndpoint(sseURL, strings.TrimSpace(data))
		s.endpointOnce.Do(func() {
			s.endpoint = endpoint
			s.endpointCh <- endpoint
		})
	case "", "message":
		// JSON-RPC response
		var resp rpcResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			return
		}
		s.dispatch(resp)
	}
}
