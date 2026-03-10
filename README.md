# opentalon-mcp

[![CI](https://github.com/opentalon/mcp-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/mcp-plugin/actions/workflows/ci.yml)

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that bridges MCP (Model Context Protocol) servers, exposing their tools to the OpenTalon AI assistant.

## Overview

`mcp-plugin` connects to one or more MCP servers over HTTP+SSE and registers their tools dynamically with OpenTalon. It runs as a plugin subprocess, communicating with the host via Unix socket or TCP gRPC.

Tools from each server are namespaced as `<server>__<tool>` (e.g. `filesystem__read_file`).

## Configuration

Set `OPENTALON_MCP_SERVERS` to a JSON array of server configs:

```json
[
  {
    "server": "filesystem",
    "url": "http://localhost:8080/sse"
  },
  {
    "server": "github",
    "url": "https://mcp.example.com/sse",
    "headers": {
      "Authorization": "Bearer {{env.GITHUB_TOKEN}}"
    }
  }
]
```

Header values support `{{env.VAR}}` expansion.

For TCP mode (e.g. Docker), set `MCP_GRPC_PORT`:

```sh
MCP_GRPC_PORT=50051 ./opentalon-mcp
```

Otherwise the plugin runs in the default Unix socket mode, launched as a subprocess by OpenTalon.

## Development

```sh
go build ./...
go test -race -count=1 -v ./...
```
