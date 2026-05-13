package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	// Close old registry's connections before replacing (prevents goroutine
	// leaks from stale SSE readLoops and "has no subscriber" log spam).
	if h.registry != nil {
		h.registry.Close()
	}
	h.registry = registry
	h.registry.StartBackgroundRetry(h.ctx)
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
	schemaTypes := make(map[string]string, len(e.schema.Properties))
	for k, prop := range e.schema.Properties {
		schemaTypes[k] = prop.Type
	}
	log.Printf("mcp-plugin: Execute call_id=%s raw_args=%v schema_types=%v", req.ID, req.Args, schemaTypes)
	args := convertArgs(req.Args, e.schema)
	coercedParts := make([]string, 0, len(args))
	for k, v := range args {
		coercedParts = append(coercedParts, fmt.Sprintf("%s=%v(%T)", k, v, v))
	}
	log.Printf("mcp-plugin: Execute call_id=%s coerced_args=[%s]", req.ID, strings.Join(coercedParts, ", "))
	// Sanitize include_fields: LLMs often include base fields (e.g. "name")
	// that are always returned. The MCP server rejects these as invalid.
	// Strip any value not in the opt-in set parsed from the parameter description.
	sanitizeIncludeFields(args, e.schema)
	log.Printf("mcp-plugin: Execute call_id=%s before CallTool server=%q tool=%q", req.ID, e.cfg.Server, e.mcpToolName)

	// Build per-request credential headers for this server from WhoAmI.
	var extraHeaders http.Header
	if cred, ok := req.CredentialHeaders[e.cfg.Server]; ok && cred.Header != "" {
		extraHeaders = http.Header{}
		extraHeaders.Set(cred.Header, cred.Value)
		log.Printf("mcp-plugin: Execute call_id=%s injecting credential header %q for server %q", req.ID, cred.Header, e.cfg.Server)
	} else {
		log.Printf("mcp-plugin: Execute call_id=%s no credential header for server %q (available: %v)", req.ID, e.cfg.Server, credKeys(req.CredentialHeaders))
	}

	content, structured, err := e.client.CallTool(e.mcpToolName, args, extraHeaders)
	if err != nil {
		log.Printf("mcp-plugin: Execute call_id=%s CallTool err: %v", req.ID, err)
		log.Printf("mcp-plugin: server %s: tool %q call failed: %v", e.cfg.Server, e.mcpToolName, err)
		return pluginpkg.Response{CallID: req.ID, Error: err.Error()}
	}
	log.Printf("mcp-plugin: Execute call_id=%s ok content_len=%d structured_len=%d", req.ID, len(content), len(structured))
	return pluginpkg.Response{CallID: req.ID, Content: content, StructuredContent: structured}
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
		result[k] = coerce(v, prop.Type, prop.Nullable)
	}
	return result
}

// coerce converts a string value to the appropriate Go type for an MCP argument.
// When nullable is true and v is the literal "null", nil is returned so the
// JSON-RPC call sends a real JSON null rather than the string "null".
func coerce(v, schemaType string, nullable bool) interface{} {
	if nullable && v == "null" {
		return nil
	}
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
	case "object":
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed
		}
	case "array":
		var parsed interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			// If the LLM sent a JSON array, use it as-is.
			if _, ok := parsed.([]interface{}); ok {
				return parsed
			}
			// JSON-parsed but not an array (e.g. a number) — wrap it.
			return []interface{}{parsed}
		}
		// Not valid JSON (e.g. bare string "all") — wrap as single-element array.
		return []interface{}{v}
	}
	return v
}

// sanitizeIncludeFields removes base-set field names from the include_fields
// argument. LLMs frequently include fields like "name" or "id" which are
// always returned by default — the MCP server rejects these as Invalid params.
// The valid opt-in fields are parsed from the parameter's description text
// (the "a subset of: field1, field2, ..." part).
func sanitizeIncludeFields(args map[string]interface{}, schema mcp.InputSchema) {
	raw, ok := args["include_fields"]
	if !ok {
		return
	}
	// Strip nil/null values — the MCP server rejects null for array fields.
	if raw == nil {
		delete(args, "include_fields")
		return
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return
	}
	if len(arr) == 0 {
		return
	}
	// Check for ["all"] — always valid, pass through.
	if len(arr) == 1 {
		if s, ok := arr[0].(string); ok && s == "all" {
			return
		}
	}
	// Parse valid opt-in fields from the parameter description.
	prop, hasProp := schema.Properties["include_fields"]
	if !hasProp {
		return
	}
	validFields := parseOptInFields(prop.Description)
	if len(validFields) == 0 {
		return // can't determine valid set, pass through as-is
	}
	// Filter to only valid opt-in fields.
	var filtered []interface{}
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if validFields[s] {
			filtered = append(filtered, s)
		} else {
			log.Printf("mcp-plugin: sanitizeIncludeFields: stripped base field %q", s)
		}
	}
	if len(filtered) == 0 {
		delete(args, "include_fields")
	} else {
		args["include_fields"] = filtered
	}
}

// parseOptInFields extracts the set of valid opt-in field names from an
// include_fields parameter description. It looks for the pattern
// "a subset of: field1, field2, field3." and returns them as a set.
func parseOptInFields(desc string) map[string]bool {
	marker := "a subset of:"
	idx := strings.Index(strings.ToLower(desc), marker)
	if idx < 0 {
		return nil
	}
	rest := desc[idx+len(marker):]
	// Take until the next period or end.
	if dot := strings.Index(rest, "."); dot >= 0 {
		rest = rest[:dot]
	}
	fields := make(map[string]bool)
	for _, f := range strings.Split(rest, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			fields[f] = true
		}
	}
	return fields
}

// credKeys returns the map keys for log output.
func credKeys(m map[string]pluginpkg.CredentialHeader) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
