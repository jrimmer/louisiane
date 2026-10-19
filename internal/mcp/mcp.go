// Package mcp is a minimal MCP server over stdio. louisiane uses it only to run
// inside Claude Code as the session's connector: it offers no tools, and
// gives Claude its louisiane instructions at initialize.
package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"sync"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Serve answers MCP requests on in/out until in ends.
func Serve(in io.Reader, out io.Writer, name, version, instructions string) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, e *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(response{JSONRPC: "2.0", ID: id, Result: result, Error: e})
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var req request
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			v := versions[0]
			if slices.Contains(versions, p.ProtocolVersion) {
				v = p.ProtocolVersion
			}
			reply(req.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": name, "version": version},
				"instructions":    instructions,
			}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": []any{}}, nil)
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		default:
			reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
		}
	}
	return sc.Err()
}
