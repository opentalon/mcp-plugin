# opentalon-mcp

[![CI](https://github.com/opentalon/mcp-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/mcp-plugin/actions/workflows/ci.yml)

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that bridges MCP (Model Context Protocol) servers, exposing their tools to the OpenTalon AI assistant.

## Overview

`mcp-plugin` connects to one or more MCP servers over Streamable HTTP, falling back to the legacy HTTP+SSE transport for servers that do not speak it, and registers their tools dynamically with OpenTalon. It runs as a plugin subprocess, communicating with the host via Unix socket or TCP gRPC.

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

### Forwarding request context as headers

`context_headers` maps a host context argument to the outbound HTTP header it
travels in. Every action of that server then declares the listed arguments in
its `InjectContextArgs`, so the host injects them before `Execute`; the plugin
pops each one out of the tool arguments and sets it as a per-request header.
The value therefore reaches the MCP server as a header and never leaks into the
tool's JSON arguments.

```json
[
  {
    "server": "inventory",
    "url": "https://mcp.example.com/mcp",
    "context_headers": {
      "session_id": "X-Session-Id",
      "interaction_kind": "X-Interaction-Kind",
      "system_source": "X-System-Source"
    }
  }
]
```

The keys are the host's own context-argument names (declared in the core's
`pkg/plugin/contextargs`); a name the host does not provide simply never
arrives. An argument the host resolves to the empty string is not forwarded and
not passed on as a tool argument either, so an MCP server sees the header only
when the value genuinely exists — absent means absent, never an invented
default. Requires a host that provides the named argument.

### Checking a call before the user confirms it

A tool whose `tools/list` entry sets `"_meta": {"check": true}` declares that
its server can check a call without executing it; the action then carries
`Check`. Only the boolean `true` declares it. Before the host asks the user to
confirm a call to such an action, it sends the call for a check: the plugin
prepares it exactly as for an execution (arguments, credential and context
headers) and sends it with the parameters of `tools/call` as the method
`opentalon/tools/check`. The check never sends `tools/call`.

| The server answers | The host |
|---|---|
| `{"verdict": "passed"}` | asks the user, as without a check |
| `{"verdict": "passed", "confirmation": "not_required"}` | executes the call without asking the user, if the host is configured to honour it (`orchestrator.pipeline.check_skips_confirmation`); otherwise asks, as for `passed` |
| `{"verdict": "refused", "kind": "…", "message": "…"}` | answers the call with the message behind the lead-in of a failed tool call (`tool <name> error: `), without asking the user and without executing it |
| an error (no such method, a tool without a check, an unknown tool), anything else, or nothing before the host's deadline | asks the user, as without a check |

A check must not change anything on the server. Only the string
`"not_required"` next to `"passed"` counts; any other value of
`confirmation`, and the field next to `"refused"`, change nothing. The action
stays a write action. Requires a host that sends checks; skipping the question
requires a host that reads `confirmation_not_required`.

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
