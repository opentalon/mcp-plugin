// Package config parses MCP server configs from the OPENTALON_MCP_SERVERS
// environment variable (JSON array) and expands {{env.X}} in header values.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// ServerConfig is one MCP server connection config, mirroring
// pkg/requestpkg.MCPServerConfig in the core repo.
type ServerConfig struct {
	Server  string            `json:"server"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

var envRe = regexp.MustCompile(`\{\{env\.(\w+)\}\}`)

// Load reads OPENTALON_MCP_SERVERS from the environment, parses the JSON
// array, and expands {{env.X}} templates in header values.
func Load() ([]ServerConfig, error) {
	raw := os.Getenv("OPENTALON_MCP_SERVERS")
	if raw == "" {
		return nil, fmt.Errorf("OPENTALON_MCP_SERVERS not set")
	}
	var cfgs []ServerConfig
	if err := json.Unmarshal([]byte(raw), &cfgs); err != nil {
		return nil, fmt.Errorf("parse OPENTALON_MCP_SERVERS: %w", err)
	}
	for i := range cfgs {
		for k, v := range cfgs[i].Headers {
			cfgs[i].Headers[k] = expandEnv(v)
		}
	}
	return cfgs, nil
}

func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(match string) string {
		name := envRe.FindStringSubmatch(match)[1]
		return os.Getenv(name)
	})
}
