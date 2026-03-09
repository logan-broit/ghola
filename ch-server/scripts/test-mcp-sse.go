//go:build ignore

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
	"time"
)

type Request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	apiURL := os.Getenv("API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}

	fmt.Println("=== Chapterhouse MCP Test ===")
	fmt.Println()

	// Step 1: Connect to SSE endpoint
	fmt.Println("1. Connecting to SSE endpoint...")
	client := &http.Client{Timeout: 0}
	req, _ := http.NewRequest("GET", apiURL+"/sse", nil)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// Read the endpoint event
	reader := bufio.NewReader(resp.Body)
	var sessionID string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			data = strings.TrimSpace(data)
			// Parse /mcp/message?session_id=xxx
			if strings.Contains(data, "session_id=") {
				sessionID = strings.Split(data, "session_id=")[1]
				break
			}
		}
	}

	if sessionID == "" {
		fmt.Println("   Error: Failed to get session ID")
		return
	}
	fmt.Printf("   Session ID: %s\n", sessionID)

	// Step 2: Initialize
	fmt.Println()
	fmt.Println("2. Sending initialize...")
	initReq := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  map[string]any{"protocolVersion": "2024-11-05"},
	}
	sendMessage(apiURL, sessionID, initReq, reader)

	// Step 3: List tools
	fmt.Println()
	fmt.Println("3. Listing tools...")
	listReq := Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	}
	sendMessage(apiURL, sessionID, listReq, reader)

	// Step 4: Call remember tool
	fmt.Println()
	fmt.Println("4. Testing remember tool...")
	rememberReq := Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: map[string]any{
			"name": "remember",
			"arguments": map[string]any{
				"fact": "The Chapterhouse project uses CloudNativePG for PostgreSQL with the pg_recall extension for vector search",
				"tags": []string{"chapterhouse", "infrastructure"},
			},
		},
	}
	sendMessage(apiURL, sessionID, rememberReq, reader)

	// Wait for embedding to be generated
	fmt.Println("   Waiting for embedding generation...")
	time.Sleep(3 * time.Second)

	// Step 5: Call recall tool
	fmt.Println()
	fmt.Println("5. Testing recall tool (semantic search)...")
	recallReq := Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params: map[string]any{
			"name": "recall",
			"arguments": map[string]any{
				"query": "what database does Chapterhouse use?",
				"mode":  "semantic",
			},
		},
	}
	sendMessage(apiURL, sessionID, recallReq, reader)

	// Step 6: List memories
	fmt.Println()
	fmt.Println("6. Listing all memories...")
	listMemReq := Request{
		JSONRPC: "2.0",
		ID:      5,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "list_memories",
			"arguments": map[string]any{},
		},
	}
	sendMessage(apiURL, sessionID, listMemReq, reader)

	fmt.Println()
	fmt.Println("=== Test complete ===")
}

func sendMessage(apiURL, sessionID string, req Request, reader *bufio.Reader) {
	body, _ := json.Marshal(req)
	httpResp, err := http.Post(
		fmt.Sprintf("%s/mcp/message?session_id=%s", apiURL, sessionID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
		return
	}
	httpResp.Body.Close()

	// Read SSE response
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("   Read error: %v\n", err)
			break
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			data = strings.TrimSpace(data)

			var resp Response
			if err := json.Unmarshal([]byte(data), &resp); err == nil {
				prettyJSON, _ := json.MarshalIndent(resp, "   ", "  ")
				fmt.Printf("   Response:\n   %s\n", string(prettyJSON))
			}
			break
		}
	}
}
