#!/bin/bash
# Test MCP tools via HTTP
# This script tests the remember, recall, and forget tools

API_URL="${API_URL:-http://localhost:8080}"

echo "=== Testing MCP Tools ==="
echo ""

# Test initialize
echo "1. Testing initialize..."
INIT_RESPONSE=$(curl -s -X POST "$API_URL/mcp" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}')
echo "   Response: $INIT_RESPONSE"

echo ""
echo "=== Testing REST API endpoints ==="
echo ""

# Test memory blocks via REST API
echo "2. Creating a memory block via REST API..."
MEMORY_RESPONSE=$(curl -s -X PUT "$API_URL/api/v1/memories/blocks/test-fact" \
  -H "Content-Type: application/json" \
  -d '{
    "tier": "index",
    "value": "[kubernetes,testing] K3s is the preferred Kubernetes runtime for local development"
  }')
echo "   Response: $MEMORY_RESPONSE"

echo ""
echo "3. Listing memory blocks..."
LIST_RESPONSE=$(curl -s "$API_URL/api/v1/memories/blocks/")
echo "   Response: $LIST_RESPONSE"

echo ""
echo "4. Health check..."
HEALTH_RESPONSE=$(curl -s "$API_URL/health")
echo "   Response: $HEALTH_RESPONSE"

echo ""
echo "=== Test complete ==="
