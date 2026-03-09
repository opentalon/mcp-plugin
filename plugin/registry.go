// Package plugin implements the OpenTalon plugin.Handler for MCP servers.
package plugin

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/opentalon/mcp-plugin/config"
	"github.com/opentalon/mcp-plugin/mcp"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

// entry maps one namespaced action name back to the client and original MCP tool name.
type entry struct {
	client      *mcp.Client
	mcpToolName string
	schema      mcp.InputSchema
}

// Registry holds all connected MCP clients and their tool mappings.
type Registry struct {
	actions map[string]entry   // key: namespaced action name, e.g. "filesystem__read_file"
	caps    pluginpkg.CapabilitiesMsg
}

// Build connects to all configured MCP servers, lists their tools, and
// builds the tool registry and capabilities message.
func Build(ctx context.Context, cfgs []config.ServerConfig) (*Registry, error) {
	r := &Registry{
		actions: make(map[string]entry),
		caps: pluginpkg.CapabilitiesMsg{
			Name:        "mcp",
			Description: "Universal MCP bridge: exposes tools from all configured MCP servers",
		},
	}

	for _, cfg := range cfgs {
		client := mcp.NewClient(cfg)
		if err := client.Connect(ctx); err != nil {
			return nil, fmt.Errorf("connect to MCP server %s: %w", cfg.Server, err)
		}

		tools, err := client.ListTools()
		if err != nil {
			return nil, fmt.Errorf("list tools from %s: %w", cfg.Server, err)
		}

		log.Printf("mcp-plugin: server %s: %d tools", cfg.Server, len(tools))

		for _, tool := range tools {
			actionName := cfg.Server + "__" + tool.Name
			r.actions[actionName] = entry{
				client:      client,
				mcpToolName: tool.Name,
				schema:      tool.InputSchema,
			}

			params := schemaToParams(tool.InputSchema)
			r.caps.Actions = append(r.caps.Actions, pluginpkg.ActionMsg{
				Name:        actionName,
				Description: tool.Description,
				Parameters:  params,
			})
		}
	}

	return r, nil
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
