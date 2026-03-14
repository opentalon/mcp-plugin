package plugin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// Handler implements pluginpkg.Handler using the MCP registry.
type Handler struct {
	registry *Registry
}

// NewHandler wraps a Registry in a Handler.
func NewHandler(r *Registry) *Handler {
	return &Handler{registry: r}
}

// Capabilities returns all namespaced MCP tools across all servers.
func (h *Handler) Capabilities() pluginpkg.CapabilitiesMsg {
	return h.registry.caps
}

// Execute routes a tool call to the correct MCP server.
func (h *Handler) Execute(req pluginpkg.Request) pluginpkg.Response {
	e, ok := h.registry.actions[req.Action]
	if !ok {
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("unknown action %q", req.Action),
		}
	}

	if e.client == nil {
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("MCP server for action %q is currently offline (loaded from cache)", req.Action),
		}
	}

	// Convert flat string args to typed interface{} map using the schema.
	args := convertArgs(req.Args, e.schema)

	content, err := e.client.CallTool(e.mcpToolName, args)
	if err != nil {
		return pluginpkg.Response{CallID: req.ID, Error: err.Error()}
	}
	return pluginpkg.Response{CallID: req.ID, Content: content}
}

// convertArgs converts map[string]string args to map[string]interface{},
// using the tool's JSON schema to decode complex types.
func convertArgs(args map[string]string, schema mcp.InputSchema) map[string]interface{} {
	result := make(map[string]interface{}, len(args))
	for k, v := range args {
		prop, hasProp := schema.Properties[k]
		if !hasProp {
			result[k] = v
			continue
		}
		result[k] = coerce(v, prop.Type)
	}
	return result
}

// coerce converts a string value to the appropriate Go type for an MCP argument.
func coerce(v, schemaType string) interface{} {
	switch strings.ToLower(schemaType) {
	case "number":
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	case "integer":
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	case "boolean":
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	case "object", "array":
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed
		}
	}
	return v
}
