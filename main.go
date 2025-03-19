// Copyright 2025 David Stotijn.
//
// Licensed under the Apache License, Version 2.0 (the "License");.
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at.
//
//     http://www.apache.org/licenses/LICENSE-2.0.
//
// Unless required by applicable law or agreed to in writing, software.
// distributed under the License is distributed on an "AS IS" BASIS,.
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and.
// limitations under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/dstotijn/go-mcp"
)

// Command-line flags.
var (
	httpAddr   string
	useStdio   bool
	useSSE     bool
	configPath string
)

func main() {
	log.SetFlags(0)

	// Get user config directory for default config path.
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		userConfigDir = ""
	}
	defaultConfigPath := filepath.Join(userConfigDir, "mcp-origin", "config.json")

	flag.StringVar(&httpAddr, "http", ":8080", "HTTP listen address for JSON-RPC over HTTP")
	flag.BoolVar(&useStdio, "stdio", true, "Enable stdio transport")
	flag.BoolVar(&useSSE, "sse", false, "Enable SSE transport")
	flag.StringVar(&configPath, "config", defaultConfigPath, "Path to the configuration file")
	flag.Parse()

	// Expand tilde in configPath if present.
	if configPath != "" && configPath[0] == '~' {
		homeDir, err := os.UserHomeDir()
		if err == nil {
			configPath = filepath.Join(homeDir, configPath[1:])
		} else {
			log.Printf("Warning: could not expand ~ in path: %v", err)
		}
	}

	// Create the config directory if it doesn't exist.
	configDir := filepath.Dir(configPath)
	if configDir != "." && configDir != "" {
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			log.Fatalf("Failed to create config directory: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	transports := []string{}
	opts := []mcp.ServerOption{}

	if useStdio {
		transports = append(transports, "stdio")
		opts = append(opts, mcp.WithStdioTransport())
	}

	var sseURL url.URL

	if useSSE {
		transports = append(transports, "sse")

		host := "localhost"

		hostPart, port, err := net.SplitHostPort(httpAddr)
		if err != nil {
			log.Fatalf("Failed to split host and port: %v", err)
		}

		if hostPart != "" {
			host = hostPart
		}

		sseURL = url.URL{
			Scheme: "http",
			Host:   host + ":" + port,
		}

		opts = append(opts, mcp.WithSSETransport(sseURL))
	}

	mcpServer := mcp.NewServer(mcp.ServerConfig{}, opts...)

	proxyManager, err := newProxyManager(configPath, mcpServer)
	if err != nil {
		log.Fatalf("Failed to create MCP proxy manager: %v", err)
	}
	defer proxyManager.close()

	tools := []mcp.Tool{
		createSearchMCPServersTool(proxyManager),
		createInstallMCPServerTool(proxyManager),
		createUninstallMCPServerTool(proxyManager),
		createRefreshToolsTool(proxyManager),
	}
	mcpServer.RegisterTools(append(tools, proxyManager.tools()...)...)

	// Load existing MCP servers from the configuration.
	if err := proxyManager.loadMCPServers(ctx); err != nil {
		log.Printf("Warning: failed to load MCP servers: %v", err)
	}

	mcpServer.Start(ctx)

	// Start HTTP server for SSE if needed.
	httpServer := &http.Server{
		Addr:        httpAddr,
		Handler:     mcpServer,
		BaseContext: func(l net.Listener) context.Context { return ctx },
	}

	if useSSE {
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server error: %v", err)
			}
		}()
	}

	log.Printf("MCP proxy server started, using transports: %v", transports)
	if useSSE {
		log.Printf("SSE transport endpoint: %v", sseURL.String())
	}

	// Wait for interrupt signal.
	<-ctx.Done()
	// Restore signal, allowing "force quit".
	stop()

	timeout := 5 * time.Second
	cancelContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Printf("Shutting down server (waiting %s). Press Ctrl+C to force quit.", timeout)

	var wg sync.WaitGroup

	if useSSE {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := httpServer.Shutdown(cancelContext); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("HTTP server shutdown error: %v", err)
			}
		}()
	}

	wg.Wait()
}

func createSearchMCPServersTool(_ *ProxyManager) mcp.Tool {
	type SearchMCPServersParams struct{}

	return mcp.CreateTool(mcp.ToolDef[SearchMCPServersParams]{
		Name:        "search_mcp_servers",
		Description: "Search for available MCP servers",
		HandleFunc: func(ctx context.Context, params SearchMCPServersParams) *mcp.CallToolResult {
			// TODO: Replace with actual MCP server definitions from an external
			// registry.
			exampleServers := []MCPServerDefinition{
				{
					ID:      "mcp-everything",
					Type:    "stdio",
					Command: "npx",
					Args:    []string{"-y", "@modelcontextprotocol/server-everything"},
				},
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Text: "Available MCP servers: ",
					},
					mcp.TextContent{
						Text: fmt.Sprintf("%+v", exampleServers),
					},
				},
			}
		},
	})
}

func createInstallMCPServerTool(manager *ProxyManager) mcp.Tool {
	return mcp.CreateTool(mcp.ToolDef[MCPServerDefinition]{
		Name:        "install_mcp_server",
		Description: "Install an MCP server",
		HandleFunc: func(ctx context.Context, params MCPServerDefinition) *mcp.CallToolResult {
			// Install the server.
			if err := manager.installMCPServer(ctx, params); err != nil {
				return newToolCallErrorResult("Failed to install MCP server: %v", err)
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Text: fmt.Sprintf("Successfully installed MCP server %q.", params.ID),
					},
				},
			}
		},
	})
}

func createUninstallMCPServerTool(manager *ProxyManager) mcp.Tool {
	type UninstallMCPServerParams struct {
		// The unique identifier for the MCP server to uninstall.
		ID string `json:"id"`
	}

	return mcp.CreateTool(mcp.ToolDef[UninstallMCPServerParams]{
		Name:        "uninstall_mcp_server",
		Description: "Uninstall an MCP server",
		HandleFunc: func(ctx context.Context, params UninstallMCPServerParams) *mcp.CallToolResult {
			if err := manager.uninstallMCPServer(ctx, params.ID); err != nil {
				return newToolCallErrorResult("Failed to uninstall MCP server: %v", err)
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Text: fmt.Sprintf("Successfully uninstalled MCP server %q.", params.ID),
					},
				},
			}
		},
	})
}

func createRefreshToolsTool(proxyManager *ProxyManager) mcp.Tool {
	type RefreshToolsParams struct {
		// Optional server ID to refresh tools for a specific server only
		ServerID string `json:"server_id,omitempty"`
	}

	return mcp.CreateTool(mcp.ToolDef[RefreshToolsParams]{
		Name:        "refresh_tools",
		Description: "Refresh the list of tools from connected MCP servers",
		HandleFunc: func(ctx context.Context, params RefreshToolsParams) *mcp.CallToolResult {
			var serverIDs []string

			// Determine which servers to refresh
			if params.ServerID != "" {
				// Check if the specified server exists
				proxyManager.mu.RLock()
				_, exists := proxyManager.proxyClients[params.ServerID]
				proxyManager.mu.RUnlock()

				if !exists {
					return newToolCallErrorResult("MCP server %q not found.", params.ServerID)
				}

				serverIDs = []string{params.ServerID}
			} else {
				// Get all server IDs
				proxyManager.mu.RLock()
				serverIDs = make([]string, 0, len(proxyManager.proxyClients))
				for id := range proxyManager.proxyClients {
					serverIDs = append(serverIDs, id)
				}
				proxyManager.mu.RUnlock()
			}

			// Refresh tools for each server
			refreshedCount := 0
			for _, id := range serverIDs {
				if err := refreshToolsForServer(ctx, proxyManager, id); err != nil {
					log.Printf("Warning: failed to refresh tools for server %q: %v", id, err)
				} else {
					refreshedCount++
				}
			}

			// Notify clients that tools have been updated
			proxyManager.registerAllTools(ctx)

			// Return a simple success message with the count of refreshed servers
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Text: fmt.Sprintf("Successfully refreshed tools for %d MCP server(s)", refreshedCount),
					},
				},
			}
		},
	})
}

// refreshToolsForServer refreshes the tools for a specific MCP server
func refreshToolsForServer(ctx context.Context, manager *ProxyManager, serverID string) error {
	manager.mu.RLock()
	client, exists := manager.proxyClients[serverID]
	manager.mu.RUnlock()

	if !exists {
		return fmt.Errorf("MCP server %q not found", serverID)
	}

	// List tools from the server
	result, err := client.c.ListTools(ctx, mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("failed to list tools: %w", err)
	}

	// Update the tools map
	manager.mu.Lock()
	client.toolsByName = make(map[string]mcp.Tool)
	for _, tool := range result.Tools {
		client.toolsByName[tool.Name] = tool
	}
	manager.mu.Unlock()

	return nil
}

func newToolCallErrorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Text: fmt.Sprintf(format, args...),
			},
		},
		IsError: true,
	}
}
