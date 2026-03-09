package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/opentalon/mcp-plugin/config"
	mcpplugin "github.com/opentalon/mcp-plugin/plugin"
	pluginpkg "github.com/opentalon/opentalon/pkg/plugin"
)

func main() {
	cfgs, err := config.Load()
	if err != nil {
		log.Fatalf("mcp-plugin: load config: %v", err)
	}
	if len(cfgs) == 0 {
		log.Fatalf("mcp-plugin: no MCP server configs found in OPENTALON_MCP_SERVERS")
	}

	ctx := context.Background()
	registry, err := mcpplugin.Build(ctx, cfgs)
	if err != nil {
		log.Fatalf("mcp-plugin: build registry: %v", err)
	}

	handler := mcpplugin.NewHandler(registry)

	// TCP mode: MCP_GRPC_PORT=50051 → listen on TCP; print handshake; serve.
	if port := os.Getenv("MCP_GRPC_PORT"); port != "" {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatalf("mcp-plugin: listen tcp :%s: %v", port, err)
		}
		hs := pluginpkg.Handshake{
			Version: pluginpkg.HandshakeVersion,
			Network: "tcp",
			Address: "0.0.0.0:" + port,
		}
		if _, err := fmt.Fprintln(os.Stdout, hs.String()); err != nil {
			log.Fatalf("mcp-plugin: write handshake: %v", err)
		}
		if err := pluginpkg.ServeListener(ln, handler); err != nil {
			log.Fatalf("mcp-plugin: serve: %v", err)
		}
		return
	}

	// Default: Unix socket mode (launched as subprocess by OpenTalon).
	if err := pluginpkg.Serve(handler); err != nil {
		log.Fatalf("mcp-plugin: serve: %v", err)
	}
}
