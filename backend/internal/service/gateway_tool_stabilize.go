package service

import (
	"sort"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stabilizeToolOrder sorts the "tools" array in the request body by tool name.
// This prevents prompt cache prefix misses caused by non-deterministic tool
// ordering when MCP tools register asynchronously between API calls.
//
// Background: Anthropic's prompt cache is prefix-based — any byte-level change
// in the tools array (which precedes system and messages in the cache prefix)
// invalidates the entire cache. When MCP tools arrive in different orders
// across calls, the cache busts on every turn.
//
// Reference: claude-code-cache-fix (github.com/cnighswonger/claude-code-cache-fix)
func stabilizeToolOrder(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}

	arr := tools.Array()
	if len(arr) <= 1 {
		return body
	}

	// Check if already sorted
	sorted := true
	for i := 1; i < len(arr); i++ {
		prev := arr[i-1].Get("name").String()
		curr := arr[i].Get("name").String()
		if prev > curr {
			sorted = false
			break
		}
	}
	if sorted {
		return body
	}

	// Sort by name
	type toolEntry struct {
		name string
		raw  string
	}
	entries := make([]toolEntry, len(arr))
	for i, t := range arr {
		entries[i] = toolEntry{
			name: t.Get("name").String(),
			raw:  t.Raw,
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})

	// Rebuild the tools array using sjson
	// Clear and rebuild to ensure correct ordering
	sortedRaws := make([]interface{}, len(entries))
	for i, e := range entries {
		sortedRaws[i] = gjson.Parse(e.raw).Value()
	}

	result, err := sjson.SetBytes(body, "tools", sortedRaws)
	if err != nil {
		return body // on error, return unchanged
	}
	return result
}
