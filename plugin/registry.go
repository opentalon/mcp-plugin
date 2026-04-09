// Package plugin implements the OpenTalon plugin.Handler for MCP servers.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	mu      sync.RWMutex
	actions map[string]entry   // key: namespaced action name, e.g. "filesystem__read_file"
	caps    pluginpkg.CapabilitiesMsg
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
	cacheDir := config.CacheDir()

	r := &Registry{
		actions: make(map[string]entry),
		caps: pluginpkg.CapabilitiesMsg{
			Name:        "mcp",
			Description: "Universal MCP bridge: exposes tools from all configured MCP servers",
		},
	}

	for _, cfg := range cfgs {
		tools, client, err := fetchTools(ctx, cfg)
		if err != nil {
			log.Printf("mcp-plugin: server %s: %v", cfg.Server, err)
			if cacheDir != "" {
				tools = loadCache(cacheDir, cfg.Server)
			}
			if len(tools) == 0 {
				log.Printf("mcp-plugin: server %s: no cache available, skipping", cfg.Server)
				continue
			}
			log.Printf("mcp-plugin: server %s: using cached spec (%d tools)", cfg.Server, len(tools))
			client = nil // mark as offline
		} else {
			log.Printf("mcp-plugin: server %s: %d tools", cfg.Server, len(tools))
			if cacheDir != "" {
				if saveErr := saveCache(cacheDir, cfg.Server, tools); saveErr != nil {
					log.Printf("mcp-plugin: server %s: save cache: %v", cfg.Server, saveErr)
				}
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
			params := schemaToParams(tool.InputSchema)
			r.caps.Actions = append(r.caps.Actions, pluginpkg.ActionMsg{
				Name:        actionName,
				Description: desc,
				Parameters:  params,
			})
		}
	}

	return r, nil
}

// fetchTools connects to one MCP server and returns its tool list plus the live client.
func fetchTools(ctx context.Context, cfg config.ServerConfig) ([]mcp.Tool, *mcp.Client, error) {
	client := mcp.NewClient(cfg)
	if err := client.Connect(ctx); err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	tools, err := client.ListTools()
	if err != nil {
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}
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
	return params
}

// reconnect creates a fresh client for the server in cfg and, on success,
// updates every action entry for that server so subsequent calls use the new connection.
// It returns the new client so the caller can proceed immediately.
func (r *Registry) reconnect(ctx context.Context, cfg config.ServerConfig) (*mcp.Client, error) {
	client := mcp.NewClient(cfg)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
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
