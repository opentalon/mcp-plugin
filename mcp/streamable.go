package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// streamableHTTP implements transport for the MCP Streamable HTTP protocol.
// Requests are POSTed directly to the endpoint and responses are read from
// the HTTP response body (application/json).
type streamableHTTP struct {
	url        string
	httpClient *http.Client
	sessionID  string
	ctx        context.Context
	cancel     context.CancelFunc
}

func newStreamableHTTP(ctx context.Context, httpClient *http.Client, url string) *streamableHTTP {
	c, cancel := context.WithCancel(ctx)
	return &streamableHTTP{
		url:        url,
		httpClient: httpClient,
		ctx:        c,
		cancel:     cancel,
	}
}

func (s *streamableHTTP) roundTrip(req rpcRequest, timeout time.Duration) (rpcResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, fmt.Errorf("build POST: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if s.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", s.sessionID)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return rpcResponse{}, fmt.Errorf("POST: HTTP %d", resp.StatusCode)
	}

	// Track session ID for subsequent requests.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.sessionID = sid
	}

	var rpcResp rpcResponse
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		rpcResp, err = decodeSSEResponse(resp.Body)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&rpcResp)
	}
	if err != nil {
		return rpcResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return rpcResp, nil
}

// decodeSSEResponse reads an SSE-framed response body and JSON-decodes the
// first "data:" payload found. MCP Streamable HTTP servers (e.g. FastMCP) may
// return Content-Type: text/event-stream even for single-response round-trips.
func decodeSSEResponse(body interface{ Read([]byte) (int, error) }) (rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var r rpcResponse
			if err := json.Unmarshal([]byte(payload), &r); err != nil {
				return rpcResponse{}, fmt.Errorf("decode SSE data: %w", err)
			}
			return r, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("read SSE stream: %w", err)
	}
	return rpcResponse{}, fmt.Errorf("no data event in SSE response")
}

func (s *streamableHTTP) notify(req rpcRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build POST: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if s.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", s.sessionID)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	_ = resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.sessionID = sid
	}
	return nil
}

func (s *streamableHTTP) context() context.Context { return s.ctx }
func (s *streamableHTTP) close()                    { s.cancel() }
