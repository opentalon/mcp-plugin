package mcp

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

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
	}
}

// connect opens the SSE stream to sseURL and starts reading events in a
// goroutine. It returns the per-session POST endpoint URL.
func (s *sseConn) connect(httpClient *http.Client, sseURL string) (string, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return "", fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("SSE connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return "", fmt.Errorf("SSE connect: HTTP %d", resp.StatusCode)
	}

	go s.readLoop(resp, sseURL)

	// Wait for the endpoint event (first event from server).
	select {
	case ep := <-s.endpointCh:
		return ep, nil
	case <-s.ctx.Done():
		return "", s.ctx.Err()
	}
}

// readLoop reads SSE events and dispatches them.
func (s *sseConn) readLoop(resp *http.Response, sseURL string) {
	defer func() { _ = resp.Body.Close() }()
	defer s.cancel()

	scanner := bufio.NewScanner(resp.Body)
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
		if err := jsonUnmarshal([]byte(data), &resp); err != nil {
			return
		}
		s.dispatch(resp)
	}
}
