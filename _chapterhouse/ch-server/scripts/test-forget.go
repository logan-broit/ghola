//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
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

	fmt.Println("=== Testing Forget Tool ===")
	fmt.Println()

	// Connect to SSE endpoint
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
			if strings.Contains(data, "session_id=") {
				sessionID = strings.Split(data, "session_id=")[1]
				break
			}
		}
	}
	fmt.Printf("   Session ID: %s\n", sessionID)

	// Initialize
	sendMessage(apiURL, sessionID, Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  map[string]any{"protocolVersion": "2024-11-05"},
	}, reader)

	// List memories before
	fmt.Println()
	fmt.Println("2. Memories before forget:")
	sendMessage(apiURL, sessionID, Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "list_memories",
			"arguments": map[string]any{},
		},
	}, reader)

	// Call forget on ID 3 (the one we created in the test)
	fmt.Println()
	fmt.Println("3. Calling forget on ID 3...")
	sendMessage(apiURL, sessionID, Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "tools/call",
		Params: map[string]any{
			"name": "forget",
			"arguments": map[string]any{
				"fact_id": 3,
			},
		},
	}, reader)

	// List memories after
	fmt.Println()
	fmt.Println("4. Memories after forget:")
	sendMessage(apiURL, sessionID, Request{
		JSONRPC: "2.0",
		ID:      4,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "list_memories",
			"arguments": map[string]any{},
		},
	}, reader)

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

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			data = strings.TrimSpace(data)

			var resp Response
			if err := json.Unmarshal([]byte(data), &resp); err == nil {
				if resp.Result != nil {
					prettyJSON, _ := json.MarshalIndent(resp.Result, "   ", "  ")
					fmt.Printf("   %s\n", string(prettyJSON))
				}
			}
			break
		}
	}
}
