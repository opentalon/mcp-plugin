// Package mcp implements the MCP HTTP+SSE client protocol.
package mcp

import "encoding/json"

// JSON-RPC 2.0 types.

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"` // nil for notifications
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP initialize params.

type initializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ClientInfo      clientInfo             `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool from tools/list.

// Tool is one MCP tool returned by tools/list.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema is the JSON Schema for a tool's input parameters.
type InputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]SchemaProp     `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
}

// SchemaProp describes one property in an InputSchema.
type SchemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

// tools/call types.

type toolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type toolsCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is one item in a tools/call response.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
