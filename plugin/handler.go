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
	log.Printf("mcp-plugin: Configure begin (before parse)")
	cfg, err := config.Parse(configJSON)
	if err != nil {
		log.Printf("mcp-plugin: Configure parse err: %v", err)
		return err
	}
	if len(cfg.Servers) == 0 {
		return fmt.Errorf("mcp-plugin: no servers in config")
	}
	log.Printf("mcp-plugin: Configure parsed servers=%d (before Build)", len(cfg.Servers))
	registry, err := Build(h.ctx, cfg.Servers)
	if err != nil {
		log.Printf("mcp-plugin: Configure Build err: %v", err)
		return err
	}
	h.registry = registry
	log.Printf("mcp-plugin: init done (Configure): registry ready servers=%d actions=%d",
		len(cfg.Servers), len(registry.actions))
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
	log.Printf("mcp-plugin: Execute begin call_id=%s action=%q", req.ID, req.Action)

	if h.registry == nil {
		log.Printf("mcp-plugin: Execute call_id=%s err: not yet configured", req.ID)
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  "mcp-plugin: not yet configured",
		}
	}

	h.registry.mu.RLock()
	e, ok := h.registry.actions[req.Action]
	h.registry.mu.RUnlock()

	if !ok {
		log.Printf("mcp-plugin: Execute call_id=%s unknown action=%q", req.ID, req.Action)
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("unknown action %q", req.Action),
		}
	}

	log.Printf("mcp-plugin: Execute call_id=%s resolved server=%q mcp_tool=%q", req.ID, e.cfg.Server, e.mcpToolName)

	if e.client == nil || !e.client.IsAlive() {
		reason := "loaded from cache (server was offline at startup)"
		if e.client != nil {
			if ctxErr := e.client.TransportContextErr(); ctxErr != nil {
				reason = fmt.Sprintf("transport context done: %v", ctxErr)
			} else {
				reason = "transport context done (unknown)"
			}
		}
		log.Printf("mcp-plugin: server %s: not alive for action %q (%s), reconnecting", e.cfg.Server, req.Action, reason)
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
	log.Printf("mcp-plugin: Execute call_id=%s before CallTool server=%q tool=%q", req.ID, e.cfg.Server, e.mcpToolName)

	content, err := e.client.CallTool(e.mcpToolName, args)
	if err != nil {
		log.Printf("mcp-plugin: Execute call_id=%s CallTool err: %v", req.ID, err)
		log.Printf("mcp-plugin: server %s: tool %q call failed: %v", e.cfg.Server, e.mcpToolName, err)
		return pluginpkg.Response{CallID: req.ID, Error: err.Error()}
	}
	log.Printf("mcp-plugin: Execute call_id=%s ok content_len=%d", req.ID, len(content))
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
