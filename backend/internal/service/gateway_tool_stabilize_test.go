package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestStabilizeToolOrder_SortsUnsortedTools(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"Grep","input_schema":{}},{"name":"Bash","input_schema":{}},{"name":"Edit","input_schema":{}}],"messages":[]}`)
	result := stabilizeToolOrder(body)

	tools := gjson.GetBytes(result, "tools").Array()
	assert.Equal(t, 3, len(tools))
	assert.Equal(t, "Bash", tools[0].Get("name").String())
	assert.Equal(t, "Edit", tools[1].Get("name").String())
	assert.Equal(t, "Grep", tools[2].Get("name").String())
}

func TestStabilizeToolOrder_AlreadySorted(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"A"},{"name":"B"},{"name":"C"}],"messages":[]}`)
	result := stabilizeToolOrder(body)
	// Should return unchanged (or equivalent)
	assert.Equal(t, string(body), string(result))
}

func TestStabilizeToolOrder_NoTools(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[]}`)
	result := stabilizeToolOrder(body)
	assert.Equal(t, string(body), string(result))
}

func TestStabilizeToolOrder_SingleTool(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"Bash"}],"messages":[]}`)
	result := stabilizeToolOrder(body)
	assert.Equal(t, string(body), string(result))
}

func TestStabilizeToolOrder_EmptyToolsArray(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tools":[],"messages":[]}`)
	result := stabilizeToolOrder(body)
	assert.Equal(t, string(body), string(result))
}

func TestStabilizeToolOrder_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"Zed","desc":"z"},{"name":"Alpha","desc":"a"}],"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	result := stabilizeToolOrder(body)

	// Tools sorted
	tools := gjson.GetBytes(result, "tools").Array()
	assert.Equal(t, "Alpha", tools[0].Get("name").String())
	assert.Equal(t, "a", tools[0].Get("desc").String())
	assert.Equal(t, "Zed", tools[1].Get("name").String())

	// Other fields preserved
	assert.Equal(t, "claude-sonnet-4-5", gjson.GetBytes(result, "model").String())
	assert.True(t, gjson.GetBytes(result, "stream").Bool())
	assert.Equal(t, "hi", gjson.GetBytes(result, "messages.0.content").String())
}

func TestStabilizeToolOrder_DeterministicAcrossCalls(t *testing.T) {
	// Simulate MCP tools registering in different orders
	body1 := []byte(`{"tools":[{"name":"mcp__server1__tool_a"},{"name":"Bash"},{"name":"mcp__server2__tool_b"}]}`)
	body2 := []byte(`{"tools":[{"name":"Bash"},{"name":"mcp__server2__tool_b"},{"name":"mcp__server1__tool_a"}]}`)

	result1 := stabilizeToolOrder(body1)
	result2 := stabilizeToolOrder(body2)

	// Both should produce the same sorted order
	tools1 := gjson.GetBytes(result1, "tools").Array()
	tools2 := gjson.GetBytes(result2, "tools").Array()

	assert.Equal(t, len(tools1), len(tools2))
	for i := range tools1 {
		assert.Equal(t, tools1[i].Get("name").String(), tools2[i].Get("name").String())
	}
}
