// Package plugin implements the OpenTalon plugin.Handler for MCP servers.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/mcp-plugin/config"
	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// entry maps one namespaced action name back to the client and original MCP tool name.
// client is nil when the entry was loaded from cache (server offline).
type entry struct {
	client      *mcp.Client
	mcpToolName string
	schema      mcp.InputSchema
	cfg         config.ServerConfig // used to reconnect when client is nil or dead
}

// Registry holds all connected MCP clients and their tool mappings.
type Registry struct {
	mu             sync.RWMutex
	actions        map[string]entry // key: namespaced action name, e.g. "filesystem__read_file"
	caps           pluginpkg.CapabilitiesMsg
	failedServers  []config.ServerConfig // servers skipped during Build (no cache available)
	offlineServers []config.ServerConfig // servers loaded from cache (client == nil)
}

// cachedServer is the on-disk format for one server's tool list.
type cachedServer struct {
	Server string     `json:"server"`
	Tools  []mcp.Tool `json:"tools"`
}

// Build connects to all configured MCP servers, lists their tools, and
// builds the tool registry and capabilities message.
// If a server is unreachable and a cache exists, the cached spec is used
// so the LLM still knows what tools are available.
func Build(ctx context.Context, cfgs []config.ServerConfig) (*Registry, error) {
	log.Printf("mcp-plugin: Build begin servers=%d", len(cfgs))
	cacheDir := config.CacheDir()

	r := &Registry{
		actions: make(map[string]entry),
		caps: pluginpkg.CapabilitiesMsg{
			Name:        "mcp",
			Description: "Universal MCP bridge: exposes tools from all configured MCP servers",
		},
	}

	var instructionSections []string
	for _, cfg := range cfgs {
		log.Printf("mcp-plugin: Build server %q url=%s (before fetchTools)", cfg.Server, cfg.URL)
		tools, client, err := fetchTools(ctx, cfg)
		if err != nil {
			log.Printf("mcp-plugin: server %s: %v", cfg.Server, err)
			if cacheDir != "" {
				tools = loadCache(cacheDir, cfg.Server)
			}
			if len(tools) == 0 {
				log.Printf("mcp-plugin: server %s: no cache available, skipping", cfg.Server)
				r.failedServers = append(r.failedServers, cfg)
				continue
			}
			log.Printf("mcp-plugin: server %s: using cached spec (%d tools)", cfg.Server, len(tools))
			r.offlineServers = append(r.offlineServers, cfg)
			client = nil // mark as offline
		} else {
			log.Printf("mcp-plugin: server %s: %d tools", cfg.Server, len(tools))
			if cacheDir != "" {
				if saveErr := saveCache(cacheDir, cfg.Server, tools); saveErr != nil {
					log.Printf("mcp-plugin: server %s: save cache: %v", cfg.Server, saveErr)
				}
			}
			if instr := client.Instructions(); instr != "" {
				instructionSections = append(instructionSections, "## "+cfg.Server+"\n"+instr)
			}
			for _, g := range client.Glossary() {
				if g.Term == "" || g.Definition == "" {
					continue
				}
				r.caps.Glossary = append(r.caps.Glossary, pluginpkg.GlossaryEntryMsg{
					Term:       g.Term,
					Definition: g.Definition,
					Category:   g.Category,
					Tags:       g.Tags,
					Synonyms:   g.Synonyms,
				})
			}
			// Forward per-section knowledge articles to the orchestrator. Each
			// becomes a "mcp-knowledge:<plugin>:<id>" record on the vector store
			// (see weaviate-plugin sync_actions handling) so the prepare-path
			// RAG can pull just the relevant section into [knowledge_context]
			// instead of the full server prose ending up in every system prompt.
			//
			// Article IDs are namespaced by MCP server name to keep IDs unique
			// across multiple bridged servers — same scheme this plugin already
			// applies to action names ("<server>__<tool>").
			for _, ka := range client.KnowledgeArticles() {
				if ka.ID == "" || ka.Title == "" || ka.Content == "" {
					continue
				}
				r.caps.KnowledgeArticles = append(r.caps.KnowledgeArticles, pluginpkg.KnowledgeArticleMsg{
					ID:      cfg.Server + "__" + ka.ID,
					Title:   ka.Title,
					Content: ka.Content,
					Tags:    ka.Tags,
				})
			}
		}

		for _, tool := range tools {
			actionName := cfg.Server + "__" + tool.Name
			r.actions[actionName] = entry{
				client:      client,
				mcpToolName: tool.Name,
				schema:      tool.InputSchema,
				cfg:         cfg,
			}

			desc := tool.Description
			if client == nil {
				desc = "[offline] " + desc
			}
			// Append output schema to description so the LLM knows the
			// expected return format for structured-output tools.
			if len(tool.OutputSchema) > 0 {
				desc += "\n\nOutput schema (return JSON matching this): " + string(tool.OutputSchema)
			}
			params := schemaToParams(tool.InputSchema)
			// Declare the context args this server wants forwarded as headers
			// so the host injects them into req.Args before Execute; the
			// handler then pops them into the configured HTTP header.
			var injectContextArgs []string
			for ctxArg := range cfg.ContextHeaders {
				injectContextArgs = append(injectContextArgs, ctxArg)
			}
			r.caps.Actions = append(r.caps.Actions, pluginpkg.ActionMsg{
				Name:              actionName,
				Description:       desc,
				Parameters:        params,
				ReadOnly:          readOnlyFromAnnotations(tool.Annotations),
				AlwaysInclude:     alwaysIncludeFromMeta(tool.Meta),
				InjectContextArgs: injectContextArgs,
			})
		}
	}

	if len(instructionSections) > 0 {
		r.caps.SystemPromptAddition = strings.Join(instructionSections, "\n\n")
	}

	log.Printf("mcp-plugin: Build done actions=%d sysprompt_bytes=%d glossary_entries=%d knowledge_articles=%d",
		len(r.actions), len(r.caps.SystemPromptAddition), len(r.caps.Glossary), len(r.caps.KnowledgeArticles))
	return r, nil
}

// fetchTools connects to one MCP server and returns its tool list plus the live client.
func fetchTools(ctx context.Context, cfg config.ServerConfig) ([]mcp.Tool, *mcp.Client, error) {
	log.Printf("mcp-plugin: fetchTools server %q: NewClient + Connect (before)", cfg.Server)
	client := mcp.NewClient(cfg)
	if err := client.Connect(ctx); err != nil {
		log.Printf("mcp-plugin: fetchTools server %q: Connect err: %v", cfg.Server, err)
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	log.Printf("mcp-plugin: fetchTools server %q: Connect ok, ListTools (before)", cfg.Server)
	tools, err := client.ListTools()
	if err != nil && client.IsStreamableHTTP() {
		// Server accepted StreamableHTTP initialize but failed on tools/list;
		// it likely only supports SSE. Fall back and retry.
		log.Printf("mcp-plugin: fetchTools server %q: ListTools err on StreamableHTTP (%v), falling back to SSE", cfg.Server, err)
		if sseErr := client.FallbackSSE(ctx); sseErr != nil {
			log.Printf("mcp-plugin: fetchTools server %q: SSE fallback err: %v", cfg.Server, sseErr)
			return nil, nil, fmt.Errorf("list tools: %w (SSE fallback: %v)", err, sseErr)
		}
		tools, err = client.ListTools()
	}
	if err != nil {
		log.Printf("mcp-plugin: fetchTools server %q: ListTools err: %v", cfg.Server, err)
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}
	log.Printf("mcp-plugin: fetchTools server %q: ListTools ok tools=%d", cfg.Server, len(tools))
	return tools, client, nil
}

// cacheFile returns the path to the cache file for the given server name.
func cacheFile(cacheDir, server string) string {
	safe := strings.ReplaceAll(server, string(filepath.Separator), "_")
	return filepath.Join(cacheDir, safe+".json")
}

// saveCache writes the tool list for a server to disk.
func saveCache(cacheDir, server string, tools []mcp.Tool) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(cachedServer{Server: server, Tools: tools})
	if err != nil {
		return err
	}
	return os.WriteFile(cacheFile(cacheDir, server), data, 0644)
}

// loadCache reads the cached tool list for a server from disk.
// Returns nil if no cache exists or it cannot be read.
func loadCache(cacheDir, server string) []mcp.Tool {
	data, err := os.ReadFile(cacheFile(cacheDir, server))
	if err != nil {
		return nil
	}
	var c cachedServer
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return c.Tools
}

// schemaToParams converts an MCP JSON Schema to OpenTalon ParameterMsg slice.
// Complex types (object, array) are mapped to type "json" — callers pass a JSON string.
// readOnlyFromAnnotations projects the upstream MCP `readOnlyHint`
// annotation onto the boolean the OpenTalon SDK consumes
// (pluginpkg.ActionMsg.ReadOnly). The orchestrator's per-call
// confirmation gate uses that flag to skip the
// "I'm about to execute X" prompt + planner-narration LLM round-trip
// for pure-query tools.
//
// Three input shapes possible from an upstream server:
//  1. No annotations object at all (Annotations == nil) — treat as
//     "no hint", which means "not declared read-only" → false.
//  2. Annotations present but no ReadOnlyHint field — same as (1).
//  3. ReadOnlyHint == true or false — passes through.
//
// Conservative default (false) is deliberate: an action whose
// authoring server didn't bother to annotate should still hit the
// confirmation gate. Skipping is opt-in, not opt-out.
func readOnlyFromAnnotations(ann *mcp.ToolAnnotation) bool {
	if ann == nil || ann.ReadOnlyHint == nil {
		return false
	}
	return *ann.ReadOnlyHint
}

// alwaysIncludeFromMeta projects an MCP server's `_meta.always_include` flag
// onto the tier flag the OpenTalon SDK consumes
// (pluginpkg.ActionMsg.AlwaysInclude). When true, the orchestrator keeps the
// action's full schema in the LLM's tools array every turn instead of leaving
// it as a name-only catalog entry the model must load via get_tool_details.
// A server sets it on the tools it wants one call away (e.g. its read/list
// tools), so they need no discovery round-trip.
//
// Conservative default (false): a missing, empty, or malformed `_meta` — or a
// server that never sets the flag — leaves the action catalog-only. Opting a
// tool into the always-loaded tier is deliberate, never accidental.
func alwaysIncludeFromMeta(meta json.RawMessage) bool {
	if len(meta) == 0 {
		return false
	}
	var m struct {
		AlwaysInclude bool `json:"always_include"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return false
	}
	return m.AlwaysInclude
}

func schemaToParams(schema mcp.InputSchema) []pluginpkg.ParameterMsg {
	if len(schema.Properties) == 0 {
		return nil
	}

	required := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		required[r] = true
	}

	params := make([]pluginpkg.ParameterMsg, 0, len(schema.Properties))
	for name, prop := range schema.Properties {
		t := mapType(prop.Type)
		params = append(params, pluginpkg.ParameterMsg{
			Name:        name,
			Description: prop.Description,
			Type:        t,
			Required:    required[name],
		})
	}
	// Sort by name: schema.Properties is a Go map, so iteration order is
	// randomized per process. A stable order keeps the serialized parameters
	// (and any downstream content hash over them) deterministic across restarts
	// and refreshes, so an unchanged tool is not seen as "changed".
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })
	return params
}

// StartBackgroundRetry starts goroutines that:
//  1. Retry servers with no cache (failedServers) — on success saves cache and
//     exits so the manager reloads the plugin with full connectivity.
//  2. Reconnect servers loaded from cache (offlineServers) — on success updates
//     registry entries in-place so tools become live without a full restart.
func (r *Registry) StartBackgroundRetry(ctx context.Context) {
	if len(r.failedServers) > 0 {
		names := make([]string, len(r.failedServers))
		for i, c := range r.failedServers {
			names[i] = c.Server
		}
		log.Printf("mcp-plugin: background retry: will retry %d server(s) with no cache: %v", len(r.failedServers), names)
		go r.retryLoop(ctx)
	}
	if len(r.offlineServers) > 0 {
		names := make([]string, len(r.offlineServers))
		for i, c := range r.offlineServers {
			names[i] = c.Server
		}
		log.Printf("mcp-plugin: background retry: will reconnect %d cached-but-offline server(s): %v", len(r.offlineServers), names)
		go r.reconnectOfflineLoop(ctx)
	}
}

func (r *Registry) retryLoop(ctx context.Context) {
	cacheDir := config.CacheDir()
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	pending := make([]config.ServerConfig, len(r.failedServers))
	copy(pending, r.failedServers)

	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		var stillFailing []config.ServerConfig
		for _, cfg := range pending {
			tools, _, err := fetchTools(ctx, cfg)
			if err != nil {
				log.Printf("mcp-plugin: background retry: server %q still unreachable: %v", cfg.Server, err)
				stillFailing = append(stillFailing, cfg)
				continue
			}
			log.Printf("mcp-plugin: background retry: server %q now reachable (%d tools), saving cache", cfg.Server, len(tools))
			if cacheDir != "" {
				if saveErr := saveCache(cacheDir, cfg.Server, tools); saveErr != nil {
					log.Printf("mcp-plugin: background retry: save cache %q: %v", cfg.Server, saveErr)
				}
			}
		}
		pending = stillFailing

		if len(pending) == 0 {
			log.Printf("mcp-plugin: background retry: all missing servers now reachable; restarting plugin for clean init")
			os.Exit(0)
		}

		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// reconnectOfflineLoop proactively reconnects servers that loaded from cache
// (client == nil). Unlike retryLoop it does NOT exit the process — it updates
// entries in-place via reconnect, strips the [offline] prefix from descriptions,
// and saves a fresh cache.
func (r *Registry) reconnectOfflineLoop(ctx context.Context) {
	cacheDir := config.CacheDir()
	backoff := 5 * time.Second
	const maxBackoff = 2 * time.Minute
	pending := make([]config.ServerConfig, len(r.offlineServers))
	copy(pending, r.offlineServers)

	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		var stillOffline []config.ServerConfig
		for _, cfg := range pending {
			client, err := r.reconnect(ctx, cfg)
			if err != nil {
				log.Printf("mcp-plugin: background reconnect: server %q still offline: %v", cfg.Server, err)
				stillOffline = append(stillOffline, cfg)
				continue
			}
			log.Printf("mcp-plugin: background reconnect: server %q now online", cfg.Server)

			// Refresh the cache with live tools.
			if cacheDir != "" {
				tools, listErr := client.ListTools()
				if listErr == nil {
					if saveErr := saveCache(cacheDir, cfg.Server, tools); saveErr != nil {
						log.Printf("mcp-plugin: background reconnect: save cache %q: %v", cfg.Server, saveErr)
					}
				}
			}

			// Strip [offline] prefix from capability descriptions.
			r.mu.Lock()
			for i, a := range r.caps.Actions {
				if strings.HasPrefix(a.Name, cfg.Server+"__") {
					r.caps.Actions[i].Description = strings.TrimPrefix(a.Description, "[offline] ")
				}
			}
			r.mu.Unlock()
		}
		pending = stillOffline

		if backoff < maxBackoff {
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	log.Printf("mcp-plugin: background reconnect: all cached-offline servers are now online")
}

// reconnect creates a fresh client for the server in cfg and, on success,
// updates every action entry for that server so subsequent calls use the new connection.
// It returns the new client so the caller can proceed immediately.
func (r *Registry) reconnect(ctx context.Context, cfg config.ServerConfig) (*mcp.Client, error) {
	log.Printf("mcp-plugin: reconnect server %q url=%s (before Connect)", cfg.Server, cfg.URL)
	client := mcp.NewClient(cfg)
	if err := client.Connect(ctx); err != nil {
		log.Printf("mcp-plugin: reconnect server %q: Connect err: %v", cfg.Server, err)
		return nil, err
	}
	log.Printf("mcp-plugin: reconnect server %q: Connect ok (before registry update)", cfg.Server)
	r.mu.Lock()
	for k, e := range r.actions {
		if e.cfg.Server == cfg.Server {
			e.client = client
			r.actions[k] = e
		}
	}
	r.mu.Unlock()
	log.Printf("mcp-plugin: server %s: reconnected", cfg.Server)
	return client, nil
}

// capsSnapshot returns the registry's capabilities safe to hand to a caller that
// reads them without holding the registry lock. reconnectOfflineLoop mutates
// action Descriptions in place (stripping the [offline] prefix) under r.mu, so
// the Actions slice is copied into a fresh backing array here — returning the
// shared slice would let that background write race with the caller's read (a
// torn string). The other caps fields are not mutated after Build, so a shallow
// struct copy is enough.
func (r *Registry) capsSnapshot() pluginpkg.CapabilitiesMsg {
	r.mu.RLock()
	defer r.mu.RUnlock()
	caps := r.caps
	if len(r.caps.Actions) > 0 {
		caps.Actions = make([]pluginpkg.ActionMsg, len(r.caps.Actions))
		copy(caps.Actions, r.caps.Actions)
	}
	return caps
}

// Close shuts down all live MCP client connections held by the registry.
// It is safe to call multiple times.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[*mcp.Client]bool)
	for _, e := range r.actions {
		if e.client != nil && !seen[e.client] {
			seen[e.client] = true
			e.client.Close()
		}
	}
}

func mapType(schemaType string) string {
	switch strings.ToLower(schemaType) {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "object", "array":
		return "json"
	default:
		return "string"
	}
}
