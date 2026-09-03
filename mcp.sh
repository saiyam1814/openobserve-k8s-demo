#!/bin/sh
# Minimal MCP client over streamable HTTP using curl.
# Usage: O2=http://<ip>:5080 AUTH='user:password' ./mcp.sh <method> ['<params-json>']
O2=${O2:?set O2, e.g. export O2=http://<loadbalancer-ip>:5080}
ORG=${ORG:-default}
TOKEN=$(printf "%s" "${AUTH:-root@example.com:Complexpass#123}" | base64)
PARAMS=${2:-'{}'}
curl -s -X POST "$O2/api/$ORG/mcp" \
  -H "Authorization: Basic $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$PARAMS}"
