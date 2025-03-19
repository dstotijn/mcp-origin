// Copyright 2025 David Stotijn
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/dstotijn/go-mcp"
)

// Config represents the configuration for the MCP Origin server.
type Config struct {
	// Map of MCP server definitions, indexed by server ID.
	MCPServers map[string]MCPServerDefinition `json:"mcpServers"`
}

// MCPServerDefinition represents the configuration needed to connect to an MCP server.
type MCPServerDefinition struct {
	// The unique identifier for the MCP server.
	ID string `json:"id"`
	// The type of connection to use (e.g., "stdio").
	Type string `json:"type"`
	// The command to execute to start the MCP server.
	Command string `json:"command"`
	// The arguments to pass to the command.
	Args []string `json:"args"`
}

// ProxyManager manages MCP server connections.
type ProxyManager struct {
	configPath   string
	config       Config
	proxyClients map[string]*ProxyClient
	mu           sync.RWMutex
	mcpServer    *mcp.Server
}

// ProxyClient represents a connection to an MCP server.
type ProxyClient struct {
	c           *mcp.Client
	def         MCPServerDefinition
	toolsByName map[string]mcp.Tool
	cancel      context.CancelFunc
}

func newProxyManager(configPath string, mcpServer *mcp.Server) (*ProxyManager, error) {
	manager := &ProxyManager{
		configPath:   configPath,
		config:       Config{MCPServers: map[string]MCPServerDefinition{}},
		proxyClients: make(map[string]*ProxyClient),
		mcpServer:    mcpServer,
	}

	// Try to load existing configuration if file exists
	if _, err := os.Stat(configPath); err == nil {
		if err := manager.loadConfig(); err != nil {
			return nil, fmt.Errorf("failed to load configuration: %w", err)
		}
	}

	return manager, nil
}

// loadConfig loads the configuration from the JSON file.
func (m *ProxyManager) loadConfig() error {
	file, err := os.Open(m.configPath)
	if err != nil {
		return fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if len(bytes) == 0 {
		// Empty file, initialize with empty config
		m.config = Config{MCPServers: map[string]MCPServerDefinition{}}
		return nil
	}

	if err := json.Unmarshal(bytes, &m.config); err != nil {
		return fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return nil
}

// saveConfig saves the current configuration to the JSON file.
func (m *ProxyManager) saveConfig() error {
	// Create parent directories if they don't exist
	dir := filepath.Dir(m.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal configuration to JSON
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file atomically using temporary file
	tempFile := m.configPath + ".tmp"
	if err := os.WriteFile(tempFile, data, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	if err := os.Rename(tempFile, m.configPath); err != nil {
		return fmt.Errorf("failed to rename temp config file: %w", err)
	}

	return nil
}

// loadMCPServers loads all MCP servers from the configuration and connects to them.
func (m *ProxyManager) loadMCPServers(ctx context.Context) error {
	m.mu.RLock()
	serverDefs := m.config.MCPServers
	m.mu.RUnlock()

	for _, def := range serverDefs {
		if err := m.connectToMCPServer(ctx, def); err != nil {
			log.Printf("Warning: failed to connect to MCP server %q: %v", def.ID, err)
		}
	}

	m.registerAllTools(ctx)

	return nil
}

// installMCPServer adds a new MCP server to the configuration and connects to it.
func (m *ProxyManager) installMCPServer(ctx context.Context, def MCPServerDefinition) error {
	// Update the configuration
	m.mu.Lock()
	// Check if server with same ID already exists
	_, exists := m.config.MCPServers[def.ID]
	// Store the server definition (either add new or update existing)
	m.config.MCPServers[def.ID] = def
	m.mu.Unlock()

	// Save the updated configuration
	if err := m.saveConfig(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if exists {
		if err := m.disconnectFromMCPServer(ctx, def.ID); err != nil {
			log.Printf("Warning: failed to disconnect from existing MCP server %q: %v", def.ID, err)
		}
	}

	if err := m.connectToMCPServer(ctx, def); err != nil {
		return fmt.Errorf("failed to connect to MCP server %q: %w", def.ID, err)
	}

	m.mcpServer.NotifyToolsListChanged(ctx)

	return nil
}

// uninstallMCPServer removes an MCP server from the configuration and disconnects from it.
func (m *ProxyManager) uninstallMCPServer(ctx context.Context, id string) error {
	if err := m.disconnectFromMCPServer(ctx, id); err != nil {
		log.Printf("Warning: failed to disconnect from MCP server %q: %v", id, err)
	}

	// Remove the server definition from the configuration
	m.mu.Lock()
	delete(m.config.MCPServers, id)
	m.mu.Unlock()

	// Save the updated configuration
	if err := m.saveConfig(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	return nil
}

// disconnectFromMCPServer disconnects from an MCP server by ID.
func (m *ProxyManager) disconnectFromMCPServer(ctx context.Context, id string) error {
	m.mu.Lock()
	client, exists := m.proxyClients[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %q not connected", id)
	}

	// Collect tool names to unregister before removing the client
	var toolsToUnregister []string
	for name := range client.toolsByName {
		toolsToUnregister = append(toolsToUnregister, proxyToolName(id, name))
	}

	if client.cancel != nil {
		client.cancel()
	}

	delete(m.proxyClients, id)
	m.mu.Unlock()

	// Unregister tools for this specific server
	if m.mcpServer != nil && len(toolsToUnregister) > 0 {
		m.mcpServer.UnregisterTools(toolsToUnregister...)
		m.mcpServer.NotifyToolsListChanged(ctx)
	}

	return nil
}

// connectToMCPServer connects to an MCP server based on its definition.
func (m *ProxyManager) connectToMCPServer(ctx context.Context, def MCPServerDefinition) error {
	m.mu.RLock()
	if _, exists := m.proxyClients[def.ID]; exists {
		return fmt.Errorf("already connected to MCP server %q", def.ID)
	}
	m.mu.RUnlock()

	switch def.Type {
	case "stdio":
		// Create a context with a cancel function to stop the child process.
		clientCtx, cancel := context.WithCancel(ctx)

		// Create an MCP client connection.
		clientConfig := mcp.ClientConfig{}
		client := mcp.NewClient(clientConfig,
			mcp.WithStdioClientTransport(mcp.StdioClientTransportConfig{
				Command: def.Command,
				Args:    def.Args,
			}),
		)

		err := client.Connect(clientCtx)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to connect to MCP server %q: %w", def.ID, err)
		}

		_, err = client.Initialize(ctx, mcp.InitializeParams{
			ProtocolVersion: mcp.ProtocolVersion,
			ClientInfo: mcp.Implementation{
				Name:    "mcp-proxy",
				Version: "0.1.0",
			},
		})
		if err != nil {
			cancel()
			return fmt.Errorf("failed to initialize MCP server %q: %w", def.ID, err)
		}

		// Initialize the client and get the list of available tools.
		var listToolsParams mcp.ListToolsParams
		result, err := client.ListTools(clientCtx, listToolsParams)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to list tools: %w", err)
		}

		// Create a map of tool name to tool.
		toolsByName := make(map[string]mcp.Tool)
		for _, tool := range result.Tools {
			toolsByName[tool.Name] = tool
		}

		// Store the client.
		m.mu.Lock()
		m.proxyClients[def.ID] = &ProxyClient{
			def:         def,
			c:           client,
			toolsByName: toolsByName,
			cancel:      cancel,
		}
		m.mu.Unlock()

		return nil
	default:
		return fmt.Errorf("unsupported MCP server type: %q", def.Type)
	}
}

func (m *ProxyManager) registerAllTools(ctx context.Context) {
	if m.mcpServer == nil {
		return
	}

	var tools []mcp.Tool
	for serverID, client := range m.proxyClients {
		for name, tool := range client.toolsByName {
			tools = append(tools, newProxyTool(client, serverID, name, tool))
		}
	}

	// Register all tools from all connected MCP servers.
	m.mcpServer.RegisterTools(tools...)

	// Notify clients that tools list has changed.
	m.mcpServer.NotifyToolsListChanged(ctx)
}

// tools returns all tools from all connected MCP servers.
func (m *ProxyManager) tools() []mcp.Tool {
	var tools []mcp.Tool

	m.mu.RLock()
	defer m.mu.RUnlock()

	for serverID, client := range m.proxyClients {
		for name, tool := range client.toolsByName {
			tools = append(tools, newProxyTool(client, serverID, name, tool))
		}
	}

	return tools
}

func proxyToolName(serverID, toolName string) string {
	return serverID + "-" + toolName
}

func newProxyTool(client *ProxyClient, serverID, toolName string, tool mcp.Tool) mcp.Tool {
	return mcp.Tool{
		Name:        proxyToolName(serverID, toolName),
		Description: fmt.Sprintf("[%v] %v", serverID, tool.Description),
		InputSchema: tool.InputSchema,
		HandleFunc: func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
			return client.callTool(ctx, toolName, args)
		},
	}
}

// close closes all MCP server connections.
func (m *ProxyManager) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel all client connections.
	for _, client := range m.proxyClients {
		if client.cancel != nil {
			client.cancel()
		}
	}

	// Clear the clients map.
	m.proxyClients = make(map[string]*ProxyClient)

	return nil
}

func (pc *ProxyClient) callTool(ctx context.Context, toolName string, params json.RawMessage) (*mcp.CallToolResult, error) {
	return pc.c.CallTool(ctx, toolName, params)
}
