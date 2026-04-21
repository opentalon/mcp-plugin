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

// UnmarshalJSON handles the JSON Schema spec where "type" may be either a
// string ("string") or an array of strings (["string", "null"]).
// When an array is provided, the first non-"null" element is used.
func (s *SchemaProp) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion.
	type schemaPropAlias struct {
		Type        json.RawMessage `json:"type"`
		Description string          `json:"description,omitempty"`
	}
	var alias schemaPropAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	s.Description = alias.Description
	if len(alias.Type) == 0 {
		return nil
	}
	// Try string first.
	if alias.Type[0] == '"' {
		return json.Unmarshal(alias.Type, &s.Type)
	}
	// Try array: pick first non-"null" element.
	var types []string
	if err := json.Unmarshal(alias.Type, &types); err != nil {
		return err
	}
	for _, t := range types {
		if t != "null" {
			s.Type = t
			return nil
		}
	}
	if len(types) > 0 {
		s.Type = types[0]
	}
	return nil
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
