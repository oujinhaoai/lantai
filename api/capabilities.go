package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"sort"
)

// AgentOperations is the reviewed adapter inventory, not an authorization table.
// Installed services are advertised by /meta; every action rechecks live policy.
//
//go:embed agent-operations.json
var AgentOperations []byte

const AgentOperationsContract = "lantai.agent-operations/v1"

type AgentOperation struct {
	ID             string   `json:"id"`
	Route          string   `json:"route,omitempty"`
	CLI            []string `json:"cli"`
	MCP            []string `json:"mcp"`
	Skill          []string `json:"skill"`
	Classification string   `json:"classification"`
	Reason         string   `json:"reason,omitempty"`
}

type AgentOperationPage struct {
	Contract   string           `json:"contract"`
	Items      []AgentOperation `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Complete   bool             `json:"complete"`
}

// AgentOperationList is a stable, bounded build inventory. Its cursor is the
// last operation ID, not a grant, a project identifier or a data query cursor.
func AgentOperationList(after string, limit int) (AgentOperationPage, error) {
	var doc struct {
		Contract   string           `json:"contract"`
		Operations []AgentOperation `json:"operations"`
	}
	if err := json.Unmarshal(AgentOperations, &doc); err != nil {
		return AgentOperationPage{}, err
	}
	if doc.Contract != AgentOperationsContract || limit < 1 || limit > 20 {
		return AgentOperationPage{}, errors.New("invalid capability page")
	}
	start := 0
	if after != "" {
		start = sort.Search(len(doc.Operations), func(i int) bool { return doc.Operations[i].ID >= after })
		if start == len(doc.Operations) || doc.Operations[start].ID != after {
			return AgentOperationPage{}, errors.New("unknown capability cursor; restart the inventory after upgrading the server")
		}
		start++
	}
	end := min(start+limit, len(doc.Operations))
	out := AgentOperationPage{Contract: doc.Contract, Items: doc.Operations[start:end], Complete: end == len(doc.Operations)}
	if !out.Complete {
		out.NextCursor = doc.Operations[end-1].ID
	}
	return out, nil
}
