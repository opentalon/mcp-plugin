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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	// Data carries the server-provided detail behind a generic Message. The
	// JSON-RPC 2.0 spec keeps the top-level Message short and stable (e.g.
	// "Invalid params") and puts the actual reason ("Missing required
	// arguments: item_id", a validation error string, …) here. Without
	// surfacing Data, every "Invalid params" / "Internal error" looks
	// identical in the orchestrator log — and the diagnostic signal lives
	// here. Forwarded raw so a server emitting a JSON object stays
	// inspectable.
	Data json.RawMessage `json:"data,omitempty"`
}

// String renders an rpcError for logging / wrapping. Includes Data when the
// server provided one, otherwise just the generic Message.
func (e rpcError) String() string {
	if len(e.Data) == 0 {
		return e.Message
	}
	// Strip surrounding quotes when Data is a JSON string so the rendered
	// form reads naturally ("Invalid params: Missing required arguments:
	// item_id") instead of escape-quoted.
	if len(e.Data) > 1 && e.Data[0] == '"' {
		var s string
		if err := json.Unmarshal(e.Data, &s); err == nil {
			return e.Message + ": " + s
		}
	}
	return e.Message + ": " + string(e.Data)
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

// initializeResult is the subset of the MCP initialize response we care about.
// `instructions` is optional per the MCP spec — when set, the server provides
// orientation prose meant for the client/LLM as system-level guidance.
// `glossary` and `knowledge_articles` are custom extensions — MCP servers can
// provide domain-specific term/definition pairs (synced to the vector store
// for context injection) and per-section reference articles (retrieved via
// the prepare-path RAG into [knowledge_context] only when relevant), keeping
// the server prompt small.
// Other fields (protocolVersion, capabilities, serverInfo) are intentionally
// omitted; we don't currently read them.
type initializeResult struct {
	Instructions      string                  `json:"instructions"`
	Glossary          []InitGlossaryEntry     `json:"glossary,omitempty"`
	KnowledgeArticles []InitKnowledgeArticle  `json:"knowledge_articles,omitempty"`
}

// InitGlossaryEntry is a term/definition pair from an MCP server's initialize response.
type InitGlossaryEntry struct {
	Term       string   `json:"term"`
	Definition string   `json:"definition"`
	Category   string   `json:"category,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Synonyms   []string `json:"synonyms,omitempty"`
}

// InitKnowledgeArticle is one self-contained reference section provided by an
// MCP server's initialize response. Bridges to the orchestrator's per-section
// RAG so that long server prose can be split out of the always-on system
// prompt and pulled in selectively per query.
type InitKnowledgeArticle struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// Tool from tools/list.

// Tool is one MCP tool returned by tools/list.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  InputSchema     `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`  // optional JSON Schema for structured output
	Annotations  *ToolAnnotation `json:"annotations,omitempty"`   // optional MCP tool annotations
	Meta         json.RawMessage `json:"_meta,omitempty"`         // optional MCP `_meta` extension bag (e.g. an always_include tier pin)
}

// ToolAnnotation carries MCP tool metadata hints.
type ToolAnnotation struct {
	Title            string `json:"title,omitempty"`
	ReadOnlyHint     *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint  *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint   *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint    *bool  `json:"openWorldHint,omitempty"`
}

// InputSchema is the JSON Schema for a tool's input parameters.
//
// Only these three keywords are modelled. A server that factors shared
// sub-schemas into a tool-level "$defs" therefore loses it: the properties
// referencing it still travel whole (SchemaProp.Raw), but their "$ref" would
// have nothing to resolve against on the far side, so the host drops such a
// property back to its type and description rather than announcing a
// reference that dangles. No server bridged so far factors its schemas that
// way; widening this struct is what to do when one does.
type InputSchema struct {
	Type       string                `json:"type"`
	Properties map[string]SchemaProp `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

// SchemaProp describes one property in an InputSchema.
//
// Type, Nullable and Description are the parts this bridge acts on itself:
// coercing a flat string argument back to its declared type, and reading the
// opt-in field list out of a description. Everything else a JSON Schema can
// say about a property — enum values, array item types, nested shapes,
// formats, defaults — lives only in Raw, which travels to the host untouched
// so the model is shown the property the server wrote rather than a summary
// of it.
type SchemaProp struct {
	Type        string          `json:"type"`
	Nullable    bool            `json:"-"` // true when JSON Schema type includes "null"
	Description string          `json:"description,omitempty"`
	Raw         json.RawMessage `json:"-"` // verbatim property JSON; re-emitted by MarshalJSON
}

// MarshalJSON writes the property back exactly as its server wrote it.
//
// The offline cache stores whole tools/list results, so without this the
// round trip through disk would narrow every cached property to type +
// description: enum values and item types gone, and ["string","null"]
// flattened to "string". A property built in code rather than parsed from a
// server carries no Raw and falls back to the struct's own fields.
//
// Value receiver on purpose: properties live in a map, whose values are not
// addressable, so a pointer method would simply never be called.
func (s SchemaProp) MarshalJSON() ([]byte, error) {
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	type schemaPropAlias SchemaProp // sheds the method set, so no recursion
	return json.Marshal(schemaPropAlias(s))
}

// UnmarshalJSON keeps the property verbatim and, on top of that, resolves the
// JSON Schema spec's two spellings of "type": a string ("string") or an array
// of strings (["string", "null"]). When an array is provided, the first
// non-"null" element is used.
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
	// Copy: the decoder's buffer is not ours to hold on to.
	s.Raw = append(json.RawMessage(nil), data...)
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
		if t == "null" {
			s.Nullable = true
		}
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
	Content           []Content       `json:"content"`
	IsError           bool            `json:"isError,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"` // MCP revision 2025-06+: schema-validated JSON payload
}

// Content is one item in a tools/call response.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
