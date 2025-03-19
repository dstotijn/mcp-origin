# 🌱 mcp-origin

One MCP server to rule (well... manage) them all.

## Features

- Connect to multiple MCP servers via a single MCP proxy
- Proxy tool calls to the appropriate server using a consistent naming scheme
- Store server configurations in a simple JSON configuration file
- Automatic tool discovery and registration from connected MCP servers

## TODO

- [ ] Use external MCP registry service for discovery
- [ ] Support SSE for proxied MCP servers

## Usage

Use the following command and args for MCP over stdio:

```
npx binrun github.com/dstotijn/mcp-origin@latest
```

## Available Tools

mcp-origin provides the following built-in tools:

### `search_mcp_servers`

Search for available MCP servers that can be installed.

> [!IMPORTANT] This tool isn't implemented yet.

### `install_mcp_server`

Install and connect to an MCP server. This tool requires the following
parameters:

```jsonc
{
  "id": "foobar",                          // Unique identifier for the MCP server
  "type": "stdio",                         // Connection type (currently only "stdio" is supported)
  "command": "npx",                        // Command to execute to start the MCP server
  "args": ["-y", "@acme/mcp-foobar", ...]  // Arguments to pass to the command
}
```

### `uninstall_mcp_server`

Remove an MCP server from the configuration and disconnect from it. This tool
requires the following parameter:

```jsonc
{
  "id": "foobar" // The ID of the server to uninstall
}
```

### `refresh_tools`

Refresh the list of tools from connected MCP servers. This tool accepts an
optional parameter:

```jsonc
{
  "server_id": "foobar" // Optional: Only refresh tools for this specific server
}
```

## Proxied Tools

All tools from connected MCP servers are available with the prefix `serverID.`,
where `serverID` is the unique identifier for the MCP server.

For example, if you have a server installed with ID `foobar` that provides a
tool called `search`, the tool will be listed and is callable as
`foobar.search`.

## Command-line Options

- `--http`: HTTP listen address (default: ":8080")
- `--stdio`: Enable stdio transport (default: true)
- `--sse`: Enable SSE transport (default: false)
- `--config`: Path to configuration file (default: "~/.config/mcp-origin/mcp_origin_config.json")

## License

[Apache-2.0 license](/LICENSE)

---

© 2025 David Stotijn
