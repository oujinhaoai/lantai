package api

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// AgentOperations is the reviewed adapter inventory, not an authorization table.
// Installed services are advertised by /meta; every action rechecks live policy.
//
//go:embed agent-operations.json
var AgentOperations []byte

const AgentOperationsContract = "lantai.agent-operations/v1"

var ErrAgentOperationCursorExpired = errors.New("capability catalog changed; restart the inventory")

type agentOperationCursor struct {
	CatalogSHA256 string `json:"catalog_sha256"`
	After         string `json:"after"`
}

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
	Contract      string           `json:"contract"`
	CatalogSHA256 string           `json:"catalog_sha256"`
	Items         []AgentOperation `json:"items"`
	NextCursor    string           `json:"next_cursor,omitempty"`
	Complete      bool             `json:"complete"`
}

// AgentOperationList is a bounded build inventory. Its opaque cursor binds the
// last operation ID to the exact catalog digest; it conveys no authorization.
func AgentOperationList(cursor string, limit int) (AgentOperationPage, error) {
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
	digest := sha256.Sum256(AgentOperations)
	catalogSHA256 := hex.EncodeToString(digest[:])
	after := ""
	if cursor != "" {
		if len(cursor) > 1024 {
			return AgentOperationPage{}, errors.New("invalid capability cursor")
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
		if err != nil {
			return AgentOperationPage{}, errors.New("invalid capability cursor")
		}
		var c agentOperationCursor
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil || c.After == "" || len(c.CatalogSHA256) != 64 {
			return AgentOperationPage{}, errors.New("invalid capability cursor")
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return AgentOperationPage{}, errors.New("invalid capability cursor")
		}
		if c.CatalogSHA256 != catalogSHA256 {
			return AgentOperationPage{}, ErrAgentOperationCursorExpired
		}
		after = c.After
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
	out := AgentOperationPage{Contract: doc.Contract, CatalogSHA256: catalogSHA256, Items: doc.Operations[start:end], Complete: end == len(doc.Operations)}
	if !out.Complete {
		raw, err := json.Marshal(agentOperationCursor{CatalogSHA256: catalogSHA256, After: doc.Operations[end-1].ID})
		if err != nil {
			return AgentOperationPage{}, err
		}
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
