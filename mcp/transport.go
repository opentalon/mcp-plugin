package mcp

import (
	"context"
	"time"
)

// transport abstracts the MCP wire protocol (SSE vs Streamable HTTP).
type transport interface {
	// roundTrip sends a JSON-RPC request and waits for the response.
	roundTrip(req rpcRequest, timeout time.Duration) (rpcResponse, error)
	// notify sends a fire-and-forget JSON-RPC notification.
	notify(req rpcRequest) error
	// context returns the transport's context (cancelled on close).
	context() context.Context
	// close shuts down the transport.
	close()
}
