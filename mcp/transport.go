package mcp

import (
	"context"
	"net/http"
	"time"
)

// transport abstracts the MCP wire protocol (SSE vs Streamable HTTP).
type transport interface {
	// roundTrip sends a JSON-RPC request and waits for the response.
	// extraHeaders are per-request credential headers that override static
	// config headers (e.g. from WhoAmI). Pass nil when not needed.
	roundTrip(req rpcRequest, timeout time.Duration, extraHeaders http.Header) (rpcResponse, error)
	// notify sends a fire-and-forget JSON-RPC notification.
	notify(req rpcRequest) error
	// context returns the transport's context (cancelled on close).
	context() context.Context
	// close shuts down the transport.
	close()
}
