package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// MCP JSON-RPC types
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var apiURL = "http://localhost:8080"

func init() {
	if url := os.Getenv("CH_API_URL"); url != "" {
		apiURL = url
	}
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	// Increase buffer size for large messages
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			sendError(nil, -32700, "Parse error")
			continue
		}

		handleRequest(req)
	}
}

func handleRequest(req JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		sendResult(req.ID, InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: Capabilities{
				Tools: &ToolsCapability{},
			},
			ServerInfo: ServerInfo{
				Name:    "chapterhouse",
				Version: "0.1.0",
			},
		})

	case "notifications/initialized":
		// No response needed for notifications

	case "tools/list":
		sendResult(req.ID, ToolsListResult{
			Tools: []Tool{
				{
					Name:        "remember",
					Description: "Store a fact or piece of information in persistent memory. Use this when you learn something important about the user's environment, infrastructure, codebases, or preferences that should be remembered across sessions.",
					InputSchema: InputSchema{
						Type: "object",
						Properties: map[string]Property{
							"fact": {
								Type:        "string",
								Description: "The fact or information to remember. Be specific and self-contained.",
							},
							"tags": {
								Type:        "string",
								Description: "Comma-separated tags for categorization (e.g., 'kubernetes,ssl,payments-service').",
							},
						},
						Required: []string{"fact"},
					},
				},
				{
					Name:        "recall",
					Description: "Search memory for relevant facts using semantic search. Use this when you need information about the user's environment, infrastructure, or past context.",
					InputSchema: InputSchema{
						Type: "object",
						Properties: map[string]Property{
							"query": {
								Type:        "string",
								Description: "Search query - can be keywords, a question, or a topic.",
							},
							"limit": {
								Type:        "string",
								Description: "Maximum number of results (default: 10).",
							},
						},
						Required: []string{"query"},
					},
				},
				{
					Name:        "forget",
					Description: "Remove a memory block by its name.",
					InputSchema: InputSchema{
						Type: "object",
						Properties: map[string]Property{
							"name": {
								Type:        "string",
								Description: "The name of the memory block to remove.",
							},
						},
						Required: []string{"name"},
					},
				},
				{
					Name:        "list_memories",
					Description: "List all stored memories, optionally filtered by tier.",
					InputSchema: InputSchema{
						Type: "object",
						Properties: map[string]Property{
							"tier": {
								Type:        "string",
								Description: "Filter by tier: core, index, or state.",
								Enum:        []string{"core", "index", "state"},
							},
						},
					},
				},
			},
		})

	case "tools/call":
		var params CallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			sendError(req.ID, -32602, "Invalid params")
			return
		}

		result := callTool(params)
		sendResult(req.ID, result)

	default:
		sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func callTool(params CallToolParams) CallToolResult {
	switch params.Name {
	case "remember":
		return handleRemember(params.Arguments)
	case "recall":
		return handleRecall(params.Arguments)
	case "forget":
		return handleForget(params.Arguments)
	case "list_memories":
		return handleListMemories(params.Arguments)
	default:
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Unknown tool: %s", params.Name)}},
			IsError: true,
		}
	}
}

func handleRemember(args map[string]any) CallToolResult {
	fact, _ := args["fact"].(string)
	if fact == "" {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: "fact is required"}},
			IsError: true,
		}
	}

	// Generate a name from the fact (first 50 chars, sanitized)
	name := sanitizeName(fact)

	// Parse tags
	tier := "index" // default tier
	tagsStr, _ := args["tags"].(string)
	if tagsStr != "" {
		// Store tags in the value for now
		fact = fmt.Sprintf("[%s] %s", tagsStr, fact)
	}

	body := map[string]any{
		"tier":  tier,
		"value": fact,
	}

	jsonBody, _ := json.Marshal(body)
	resp, err := http.NewRequest("PUT", fmt.Sprintf("%s/api/v1/memories/blocks/%s", apiURL, name), bytes.NewReader(jsonBody))
	if err != nil {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}
	}
	resp.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	httpResp, err := client.Do(resp)
	if err != nil {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(httpResp.Body)
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %s", string(body))}},
			IsError: true,
		}
	}

	var result map[string]any
	json.NewDecoder(httpResp.Body).Decode(&result)

	return CallToolResult{
		Content: []Content{{Type: "text", Text: fmt.Sprintf("Remembered (name=%s): %s", name, fact)}},
	}
}

func handleRecall(args map[string]any) CallToolResult {
	query, _ := args["query"].(string)
	if query == "" {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: "query is required"}},
			IsError: true,
		}
	}

	// For now, list all blocks and filter client-side
	// TODO: Use vector search when implemented
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/memories/blocks", apiURL))
	if err != nil {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}
	}
	defer resp.Body.Close()

	var blocks []map[string]any
	json.NewDecoder(resp.Body).Decode(&blocks)

	// Simple keyword matching for now
	queryLower := strings.ToLower(query)
	var matches []string
	for _, block := range blocks {
		value, _ := block["value"].(string)
		name, _ := block["name"].(string)
		if strings.Contains(strings.ToLower(value), queryLower) ||
			strings.Contains(strings.ToLower(name), queryLower) {
			matches = append(matches, fmt.Sprintf("[%s] %s", name, value))
		}
	}

	if len(matches) == 0 {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: "No matching memories found"}},
		}
	}

	return CallToolResult{
		Content: []Content{{Type: "text", Text: strings.Join(matches, "\n")}},
	}
}

func handleForget(args map[string]any) CallToolResult {
	name, _ := args["name"].(string)
	if name == "" {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: "name is required"}},
			IsError: true,
		}
	}

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/v1/memories/blocks/%s", apiURL, name), nil)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Removed memory: %s", name)}},
		}
	}

	body, _ := io.ReadAll(resp.Body)
	return CallToolResult{
		Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %s", string(body))}},
		IsError: true,
	}
}

func handleListMemories(args map[string]any) CallToolResult {
	url := fmt.Sprintf("%s/api/v1/memories/blocks", apiURL)
	if tier, ok := args["tier"].(string); ok && tier != "" {
		url += "?tier=" + tier
	}

	resp, err := http.Get(url)
	if err != nil {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}
	}
	defer resp.Body.Close()

	var blocks []map[string]any
	json.NewDecoder(resp.Body).Decode(&blocks)

	if len(blocks) == 0 {
		return CallToolResult{
			Content: []Content{{Type: "text", Text: "No memories found"}},
		}
	}

	var lines []string
	for _, block := range blocks {
		name, _ := block["name"].(string)
		tier, _ := block["tier"].(string)
		value, _ := block["value"].(string)
		lines = append(lines, fmt.Sprintf("[%s] (%s) %s", name, tier, value))
	}

	return CallToolResult{
		Content: []Content{{Type: "text", Text: strings.Join(lines, "\n")}},
	}
}

func sanitizeName(s string) string {
	// Take first 50 chars, replace spaces/special chars with underscores
	if len(s) > 50 {
		s = s[:50]
	}
	var result strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			result.WriteRune(c)
		} else if c == ' ' {
			result.WriteRune('_')
		}
	}
	return strings.ToLower(result.String())
}

func sendResult(id any, result any) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}

func sendError(id any, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}
