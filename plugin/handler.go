package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/opentalon/mcp-plugin/config"
	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// Handler implements pluginpkg.Handler (and pluginpkg.Configurable) using the MCP registry.
type Handler struct {
	ctx      context.Context
	registry *Registry
}

// NewHandler creates a Handler. The registry is nil until Configure is called
// or SetRegistry is used directly (e.g. when bootstrapping from the env var).
func NewHandler(ctx context.Context) *Handler {
	return &Handler{ctx: ctx}
}

// SetRegistry sets the registry directly, used for env-var bootstrapping.
func (h *Handler) SetRegistry(r *Registry) {
	h.registry = r
}

// Configure implements pluginpkg.Configurable. It is called by the host via
// the Init RPC before any Execute calls, with the JSON-encoded config block
// from the host's config.yaml.
func (h *Handler) Configure(configJSON string) error {
	cfg, err := config.Parse(configJSON)
	if err != nil {
		return err
	}
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("mcp-plugin: no servers in config")
	}
	registry, err := Build(h.ctx, cfg.Servers)
	if err != nil {
		return err
	}
	h.registry = registry
	return nil
}

// Capabilities returns all namespaced MCP tools across all servers.
func (h *Handler) Capabilities() pluginpkg.CapabilitiesMsg {
	if h.registry == nil {
		return pluginpkg.CapabilitiesMsg{
			Name:        "mcp",
			Description: "Universal MCP bridge: exposes tools from all configured MCP servers",
		}
	}
	return h.registry.caps
}

// Execute routes a tool call to the correct MCP server.
func (h *Handler) Execute(req pluginpkg.Request) pluginpkg.Response {
	if h.registry == nil {
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  "mcp-plugin: not yet configured",
		}
	}

	h.registry.mu.RLock()
	e, ok := h.registry.actions[req.Action]
	h.registry.mu.RUnlock()

	if !ok {
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("unknown action %q", req.Action),
		}
	}

	if e.client == nil || !e.client.IsAlive() {
		log.Printf("mcp-plugin: server %s: offline or disconnected for action %q, reconnecting", e.cfg.Server, req.Action)
		client, err := h.registry.reconnect(h.ctx, e.cfg)
		if err != nil {
			log.Printf("mcp-plugin: server %s: reconnect failed: %v", e.cfg.Server, err)
			return pluginpkg.Response{
				CallID: req.ID,
				Error:  fmt.Sprintf("MCP server %q is offline (reconnect failed: %v)", e.cfg.Server, err),
			}
		}
		e.client = client
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
