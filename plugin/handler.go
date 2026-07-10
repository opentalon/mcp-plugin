package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/mcp-plugin/config"
	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// refreshGraceCloseDelay is how long a replaced registry's connections stay
// open after a refresh swaps in a freshly built one, so an in-flight tool call
// (CallTool round-trips with a 60s timeout) finishes on the old connection
// instead of being cut off.
const refreshGraceCloseDelay = 70 * time.Second

// Handler implements pluginpkg.Handler, pluginpkg.Configurable and
// pluginpkg.Refreshable using the MCP registry.
type Handler struct {
	ctx context.Context

	// mu guards the mutable handler state below. registry is swapped under mu
	// on each refresh; readers (Capabilities, Execute) load the pointer under
	// RLock and then use the registry's own lock for its internal fields.
	mu         sync.RWMutex
	registry   *Registry
	serverCfgs []config.ServerConfig // captured at Configure/bootstrap, replayed by RefreshCapabilities
	refreshing bool                  // true while a refresh Build is in flight; coalesces concurrent calls
}

// Compile-time assertion that Handler satisfies the host's optional Refreshable
// interface. If the core SDK's signature ever drifts, this fails the build here
// rather than silently disabling refresh (the host's type assertion would just
// miss and fall back to Unimplemented).
var _ pluginpkg.Refreshable = (*Handler)(nil)

// NewHandler creates a Handler. The registry is nil until Configure is called
// or SetRegistry is used directly (e.g. when bootstrapping from the env var).
func NewHandler(ctx context.Context) *Handler {
	return &Handler{ctx: ctx}
}

// SetRegistry sets the registry directly, used for env-var bootstrapping.
// serverCfgs are stored so RefreshCapabilities can rebuild from the same
// configuration without a host Init RPC.
func (h *Handler) SetRegistry(r *Registry, serverCfgs []config.ServerConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registry = r
	h.serverCfgs = serverCfgs
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
	h.mu.Lock()
	old := h.registry
	h.registry = registry
	h.serverCfgs = cfg.Servers
	h.mu.Unlock()

	registry.StartBackgroundRetry(h.ctx)
	// A re-Configure replaces a live registry; retire its connections gracefully
	// so any in-flight tool call finishes first (at first startup old is nil).
	if old != nil {
		h.graceCloseRegistry(old)
	}
	log.Printf("mcp-plugin: init done (Configure): registry ready servers=%d actions=%d",
		len(cfg.Servers), len(registry.actions))
	return nil
}

// Capabilities returns the current (cached) namespaced MCP tools across all
// servers. It is a pure read; the host refreshes the cache via
// RefreshCapabilities.
func (h *Handler) Capabilities() pluginpkg.CapabilitiesMsg {
	h.mu.RLock()
	reg := h.registry
	h.mu.RUnlock()
	if reg == nil {
		return defaultCaps()
	}
	return reg.capsSnapshot()
}

// RefreshCapabilities re-fetches capabilities live from the upstream MCP servers
// and atomically swaps in a freshly built registry, returning the fresh set. It
// implements pluginpkg.Refreshable: the host calls it on its periodic
// corpus-sync poll so upstream changes (tool descriptions, server instructions,
// knowledge articles) propagate without a plugin restart.
//
// Concurrent calls are coalesced via the refreshing flag: a call that finds a
// refresh already in flight returns the current capabilities instead of
// starting a second Build. On a build failure the previous capabilities are
// kept and returned — a transient upstream outage never empties the set.
func (h *Handler) RefreshCapabilities() pluginpkg.CapabilitiesMsg {
	h.mu.Lock()
	if h.refreshing || h.registry == nil || len(h.serverCfgs) == 0 {
		reg := h.registry
		h.mu.Unlock()
		if reg == nil {
			return defaultCaps()
		}
		return reg.capsSnapshot()
	}
	h.refreshing = true
	cfgs := h.serverCfgs
	prevActions := len(h.registry.caps.Actions)
	h.mu.Unlock()
	// Always clear the in-flight flag, even if Build panics — otherwise a panic
	// would leave refreshing == true and wedge every later refresh into the
	// coalescing path permanently. The flag covers the whole operation (build +
	// swap), so a concurrent call coalesces until this one fully returns.
	defer func() {
		h.mu.Lock()
		h.refreshing = false
		h.mu.Unlock()
	}()

	log.Printf("mcp-plugin: refresh: re-fetching capabilities from %d server(s)", len(cfgs))
	newReg, err := Build(h.ctx, cfgs)

	h.mu.Lock()
	if err != nil {
		reg := h.registry
		h.mu.Unlock()
		log.Printf("mcp-plugin: refresh: build failed, keeping previous capabilities: %v", err)
		return reg.capsSnapshot()
	}
	// Don't swap to a degraded build. If any server fell back to its cache
	// (offline) or had no cache at all (failed), this refresh didn't get a fully
	// live view — swapping would push [offline]-prefixed / stale descriptions to
	// the host and on into the corpus, churning every doc on a transient upstream
	// blip and blanking it again on recovery. Keep the last good live
	// capabilities; a later refresh swaps once the upstream is fully reachable.
	// This is all-or-nothing across servers: with multiple upstreams, one
	// persistently unreachable server holds back propagation from the healthy
	// ones until it recovers — an accepted trade-off for the current
	// single-upstream setup, revisit if more servers are bridged.
	if len(newReg.failedServers) > 0 || len(newReg.offlineServers) > 0 {
		reg := h.registry
		h.mu.Unlock()
		log.Printf("mcp-plugin: refresh: upstream degraded (%d offline, %d unreachable), keeping previous capabilities",
			len(newReg.offlineServers), len(newReg.failedServers))
		newReg.Close()
		return reg.capsSnapshot()
	}
	old := h.registry
	h.registry = newReg
	h.mu.Unlock()

	log.Printf("mcp-plugin: refresh: done actions=%d (was %d) sysprompt_bytes=%d glossary=%d knowledge=%d",
		len(newReg.caps.Actions), prevActions, len(newReg.caps.SystemPromptAddition),
		len(newReg.caps.Glossary), len(newReg.caps.KnowledgeArticles))

	// Only fully-live builds reach here, so there are no offline/failed servers
	// left to retry — we deliberately don't start new background-retry loops on
	// refresh (they'd accumulate across refreshes); a degraded upstream is handled
	// by the guard above plus the next periodic refresh.
	if old != nil {
		h.graceCloseRegistry(old)
	}
	return newReg.capsSnapshot()
}

// graceCloseRegistry closes a replaced registry's connections after a grace
// delay, so in-flight tool calls on the old connections finish first.
func (h *Handler) graceCloseRegistry(old *Registry) {
	go func() {
		select {
		case <-h.ctx.Done():
		case <-time.After(refreshGraceCloseDelay):
		}
		log.Printf("mcp-plugin: refresh: closing previous connections after %s grace", refreshGraceCloseDelay)
		old.Close()
	}()
}

// defaultCaps is the capability set served before the registry is configured.
func defaultCaps() pluginpkg.CapabilitiesMsg {
	return pluginpkg.CapabilitiesMsg{
		Name:        "mcp",
		Description: "Universal MCP bridge: exposes tools from all configured MCP servers",
	}
}

// Execute routes a tool call to the correct MCP server.
func (h *Handler) Execute(req pluginpkg.Request) pluginpkg.Response {
	log.Printf("mcp-plugin: Execute begin call_id=%s action=%q", req.ID, req.Action)

	h.mu.RLock()
	reg := h.registry
	h.mu.RUnlock()
	if reg == nil {
		log.Printf("mcp-plugin: Execute call_id=%s err: not yet configured", req.ID)
		return pluginpkg.Response{
			CallID: req.ID,
			Error:  "mcp-plugin: not yet configured",
		}
	}

	reg.mu.RLock()
	e, ok := reg.actions[req.Action]
	reg.mu.RUnlock()

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
		client, err := reg.reconnect(h.ctx, e.cfg)
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
	// Resolve namespaced tool names in args (e.g. default_tool_name, tool_name
	// inside workorder tasks). The LLM uses "server__tool" but the MCP server
	// only knows "tool".
	h.resolveToolNameArgs(args)
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

	// Forward configured context args (injected by the host into req.Args)
	// as HTTP headers, and remove them from the tool arguments so they are
	// not passed to the downstream tool as parameters.
	for ctxArg, headerName := range e.cfg.ContextHeaders {
		raw, ok := args[ctxArg]
		if !ok {
			continue
		}
		if val, isStr := raw.(string); isStr && headerName != "" && val != "" {
			if extraHeaders == nil {
				extraHeaders = http.Header{}
			}
			extraHeaders.Set(headerName, val)
			log.Printf("mcp-plugin: Execute call_id=%s forwarding context arg %q as header %q for server %q", req.ID, ctxArg, headerName, e.cfg.Server)
		} else {
			// Present but not forwardable (empty header name, empty or
			// non-string value). Still stripped so it can't leak into the
			// tool args; logged so a misconfigured mapping isn't silent.
			log.Printf("mcp-plugin: Execute call_id=%s dropping context arg %q without forwarding (empty header name or non-string value) for server %q", req.ID, ctxArg, e.cfg.Server)
		}
		delete(args, ctxArg)
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

// resolveToolNameArgs converts a namespaced tool name the LLM supplied as an
// argument VALUE (e.g. "timly__delete-item") to the raw MCP tool name the server
// expects (e.g. "delete-item"). It covers the tool-name-carrying args of the
// batch-job tools: default_tool_name and per-task tool_name (submit-batch-job)
// and the tool_name filter (list-batch-jobs).
//
// The value can arrive in three shapes, all resolved here:
//   - the raw name ("delete-item") — left untouched, the server already knows it;
//   - the "<server>__<tool>" action key ("timly__delete-item");
//   - the canonical "<plugin>__<server>__<tool>" FQN the LLM actually sees, since
//     the core prefixes the action key with the plugin name
//     ("timly__timly__delete-item"). Missing this form is why a batch task's
//     tool_name reached the server double-prefixed and was rejected.
//
// A value that resolves to no known action is left unchanged, so a non-tool
// string is never rewritten.
func (h *Handler) resolveToolNameArgs(args map[string]interface{}) {
	h.mu.RLock()
	reg := h.registry
	h.mu.RUnlock()
	if reg == nil {
		return
	}
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	resolve := func(v string) (string, bool) {
		if e, found := reg.actions[v]; found {
			return e.mcpToolName, true
		}
		// Strip the leading "<segment>__" the core adds on top of the action
		// key, recovering the "<server>__<tool>" key from the canonical FQN.
		if i := strings.Index(v, "__"); i >= 0 {
			if e, found := reg.actions[v[i+2:]]; found {
				return e.mcpToolName, true
			}
		}
		return "", false
	}

	// Top-level tool-name args: default_tool_name (submit-batch-job) and
	// tool_name (list-batch-jobs filter).
	for _, key := range []string{"default_tool_name", "tool_name"} {
		if v, ok := args[key].(string); ok {
			if raw, found := resolve(v); found {
				args[key] = raw
			}
		}
	}

	// Per-task tool_name inside the tasks array (submit-batch-job).
	tasks, ok := args["tasks"].([]interface{})
	if !ok {
		return
	}
	for _, t := range tasks {
		task, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := task["tool_name"].(string); ok {
			if raw, found := resolve(v); found {
				task["tool_name"] = raw
			}
		}
	}
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
