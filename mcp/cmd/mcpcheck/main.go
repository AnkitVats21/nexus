package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type authTransport struct {
	token string
	base  http.RoundTripper
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if a.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+a.token)
	}
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

func main() {
	endpointFlag := flag.String("endpoint", "", "MCP endpoint URL (default: http://127.0.0.1:8787/mcp)")
	tokenFlag := flag.String("token", "", "Bearer token (default: NEXUS_MCP_TOKEN env var)")
	toolFlag := flag.String("tool", "", "Optional tool name to call (e.g. news_headlines, weather, web_search)")
	argsFlag := flag.String("args", "{}", "JSON string of tool arguments (e.g. '{\"location\":\"London\"}')")
	timeoutFlag := flag.Duration("timeout", 20*time.Second, "Client operation timeout")

	flag.Parse()

	endpoint := strings.TrimSpace(*endpointFlag)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("NEXUS_MCP_ENDPOINT"))
	}
	if endpoint == "" {
		if addr := strings.TrimSpace(os.Getenv("NEXUS_MCP_ADDR")); addr != "" {
			if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
				endpoint = "http://" + addr + "/mcp"
			} else {
				endpoint = addr + "/mcp"
			}
		} else {
			endpoint = "http://127.0.0.1:8787/mcp"
		}
	}

	token := strings.TrimSpace(*tokenFlag)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("NEXUS_MCP_TOKEN"))
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "Error: Token must be provided via -token or NEXUS_MCP_TOKEN env var")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	fmt.Printf("Connecting to MCP endpoint %s...\n", endpoint)

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "mcpcheck",
		Version: "1.0.0",
	}, nil)

	authedClient := &http.Client{
		Timeout: *timeoutFlag,
		Transport: &authTransport{
			token: token,
		},
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           authedClient,
		DisableStandaloneSSE: true,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	fmt.Printf("Connected successfully. Listing available tools:\n\n")

	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list tools: %v\n", err)
		os.Exit(1)
	}

	for i, tool := range toolsResult.Tools {
		fmt.Printf("[%d] %s\n", i+1, tool.Name)
		fmt.Printf("    Description: %s\n", tool.Description)
		if tool.InputSchema != nil {
			if schemaBytes, err := json.Marshal(tool.InputSchema); err == nil {
				fmt.Printf("    InputSchema: %s\n", string(schemaBytes))
			}
		}
		fmt.Println()
	}

	if *toolFlag == "" {
		fmt.Println("No tool call requested. Use -tool <name> -args '<json>' to invoke a tool.")
		return
	}

	toolName := *toolFlag
	var args map[string]any
	if err := json.Unmarshal([]byte(*argsFlag), &args); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid JSON in -args: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Calling tool %q with arguments: %s\n", toolName, *argsFlag)
	callResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "CallTool RPC error: %v\n", err)
		os.Exit(1)
	}

	if callResult.IsError {
		fmt.Printf("\n[TOOL ERROR] Tool execution failed:\n")
		for _, c := range callResult.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				fmt.Printf("  %s\n", tc.Text)
			}
		}
		os.Exit(2)
	}

	fmt.Printf("\n[RESULT] Success:\n")
	for _, c := range callResult.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			fmt.Println(tc.Text)
		}
	}

	if callResult.StructuredContent != nil {
		if pretty, err := json.MarshalIndent(callResult.StructuredContent, "", "  "); err == nil {
			fmt.Printf("\nStructured Content:\n%s\n", string(pretty))
		}
	}
}
