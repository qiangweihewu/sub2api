package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestObfuscateSensitiveWords_InsertsExactlyOneZeroWidthSpace verifies the
// core CLIProxyAPI cloak technique: each match gets one zero-width space
// inserted between rune 1 and rune 2 — no more, no less.
func TestObfuscateSensitiveWords_InsertsExactlyOneZeroWidthSpace(t *testing.T) {
	body := []byte(`{
		"system":[{"type":"text","text":"You run inside OpenClaw."}],
		"messages":[{"role":"user","content":[{"type":"text","text":"Two mentions: OpenClaw and OpenClaw again."}]}]
	}`)

	out := ObfuscateSensitiveWords(body, DefaultSensitiveWords)

	// Original substring must be broken.
	if strings.Contains(string(out), "OpenClaw") {
		t.Fatalf("raw OpenClaw substring still present after obfuscation: %s", string(out))
	}
	// The obfuscated form O<ZWSP>penClaw must appear — 3 times total (1 in system + 2 in messages).
	count := strings.Count(string(out), "O"+zeroWidthSpace+"penClaw")
	if count != 3 {
		t.Fatalf("expected 3 obfuscated occurrences, got %d. body=%s", count, string(out))
	}
}

// TestObfuscateSensitiveWords_Idempotent checks that re-running obfuscation
// on already-obfuscated text does not stack extra zero-width spaces.
func TestObfuscateSensitiveWords_Idempotent(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"OpenClaw"}]}`)
	first := ObfuscateSensitiveWords(body, DefaultSensitiveWords)
	second := ObfuscateSensitiveWords(first, DefaultSensitiveWords)
	if string(first) != string(second) {
		t.Fatalf("obfuscation not idempotent.\nfirst=%q\nsecond=%q", first, second)
	}
	// Should have exactly one ZWSP, not two.
	text := gjson.GetBytes(second, "system.0.text").String()
	if strings.Count(text, zeroWidthSpace) != 1 {
		t.Fatalf("expected exactly 1 ZWSP after double-apply, got %d; text=%q", strings.Count(text, zeroWidthSpace), text)
	}
}

// TestObfuscateSensitiveWords_CaseInsensitivePreservesCasing verifies that
// matching ignores case but the inserted ZWSP preserves the original casing
// at the insertion point.
func TestObfuscateSensitiveWords_CaseInsensitivePreservesCasing(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"opencode OpenCode OPENCODE"}]}`)
	out := ObfuscateSensitiveWords(body, DefaultSensitiveWords)
	text := gjson.GetBytes(out, "system.0.text").String()
	// Each occurrence has its own casing preserved; each gets one ZWSP.
	for _, want := range []string{"o" + zeroWidthSpace + "pencode", "O" + zeroWidthSpace + "penCode", "O" + zeroWidthSpace + "PENCODE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in output, got %q", want, text)
		}
	}
}

// TestStripThirdPartyMarkers_RemovesSystemInstructions covers the literal
// `[System Instructions]` marker (the wrapper added by rewriteSystemForNonClaudeCode).
func TestStripThirdPartyMarkers_RemovesSystemInstructions(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"[System Instructions]\nYou are a helper."}]}]}`)
	out := StripThirdPartyMarkers(body)
	got := gjson.GetBytes(out, "messages.0.content.0.text").String()
	if strings.Contains(got, "[System Instructions]") {
		t.Fatalf("marker not stripped: %q", got)
	}
	if !strings.Contains(got, "You are a helper.") {
		t.Fatalf("legitimate content got nuked: %q", got)
	}
}

// TestStripThirdPartyMarkers_RemovesXMLTags covers the Meridian-style paired
// tag stripping for env / tool_exec / task_metadata etc.
func TestStripThirdPartyMarkers_RemovesXMLTags(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"before<env>OS=Linux\npwd=/tmp</env>middle<tool_exec>bash -c ls</tool_exec><task_metadata id=\"x\">foo</task_metadata>after"}]}]}`)
	out := StripThirdPartyMarkers(body)
	got := gjson.GetBytes(out, "messages.0.content.0.text").String()
	for _, banned := range []string{"<env>", "</env>", "<tool_exec>", "</tool_exec>", "<task_metadata", "</task_metadata>", "OS=Linux", "bash -c ls"} {
		if strings.Contains(got, banned) {
			t.Fatalf("XML content leaked: %q remains. got=%q", banned, got)
		}
	}
	// Non-tag content preserved.
	for _, want := range []string{"before", "middle", "after"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q, got=%q", want, got)
		}
	}
}

// TestStripThirdPartyMarkers_RemovesToolingSection verifies the OpenClaw
// `\n## Tooling\n- read: ...\n- write: ...\n\n` section is excised.
func TestStripThirdPartyMarkers_RemovesToolingSection(t *testing.T) {
	text := "before\n## Tooling\n- read: Read file contents\n- write: Create or overwrite files\n- exec: Run shell commands\n\nafter"
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		},
	})
	out := StripThirdPartyMarkers(body)
	got := gjson.GetBytes(out, "messages.0.content.0.text").String()
	if strings.Contains(got, "## Tooling") {
		t.Fatalf("tooling section not removed: %q", got)
	}
	if strings.Contains(got, "- read:") || strings.Contains(got, "- exec:") {
		t.Fatalf("tooling bullets leaked: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("boundary content lost: %q", got)
	}
}

// TestRemapToolNames_RenamesToolsAndToolUse covers the core rename path:
// tools[].name AND messages[].content[].tool_use.name in the same body.
func TestRemapToolNames_RenamesToolsAndToolUse(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"name":"read","description":"read files"},
			{"name":"write","description":"write files"},
			{"name":"exec","description":"run commands"}
		],
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"read","input":{"path":"/a"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}
			]}
		]
	}`)

	out, remap := RemapToolNames(body)

	// tools[] renamed.
	names := []string{
		gjson.GetBytes(out, "tools.0.name").String(),
		gjson.GetBytes(out, "tools.1.name").String(),
		gjson.GetBytes(out, "tools.2.name").String(),
	}
	want := []string{"Read", "Write", "Bash"}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("tools[%d].name = %q, want %q", i, names[i], w)
		}
	}
	// tool_use renamed.
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "Read" {
		t.Fatalf("tool_use.name = %q, want Read", got)
	}
	// remap table populated.
	if remap["read"] != "Read" || remap["write"] != "Write" || remap["exec"] != "Bash" {
		t.Fatalf("remap table incomplete: %+v", remap)
	}
}

// TestRemapToolNames_DropsBlocklistedAndRemovesReferences is the gnarly one:
// music_generate must be removed from tools[], its tool_use must be removed
// from assistant content, and the matching tool_result in the user turn must
// also be removed (matched by tool_use_id). Empty content arrays get replaced
// with an "[omitted tool call]" placeholder to avoid Anthropic rejecting
// zero-block content.
func TestRemapToolNames_DropsBlocklistedAndRemovesReferences(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"name":"read"},
			{"name":"music_generate","description":"generate music"}
		],
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_music","name":"music_generate","input":{"prompt":"jazz"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_music","content":"music.mp3"}
			]},
			{"role":"assistant","content":[
				{"type":"text","text":"Here is your music."},
				{"type":"tool_use","id":"tu_music_2","name":"music_generate","input":{"prompt":"rock"}}
			]}
		]
	}`)

	out, _ := RemapToolNames(body)

	// tools[] should have exactly one entry: Read (music_generate dropped).
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool after drop, got %d: %s", len(tools), gjson.GetBytes(out, "tools").Raw)
	}
	if tools[0].Get("name").String() != "Read" {
		t.Fatalf("remaining tool name = %q, want Read", tools[0].Get("name").String())
	}

	// Message 0: content was only the dropped tool_use → placeholder.
	msg0 := gjson.GetBytes(out, "messages.0.content").Array()
	if len(msg0) != 1 {
		t.Fatalf("msg0.content len = %d, want 1 (placeholder)", len(msg0))
	}
	if msg0[0].Get("type").String() != "text" || !strings.Contains(msg0[0].Get("text").String(), "omitted tool call") {
		t.Fatalf("msg0 placeholder not inserted: %s", msg0[0].Raw)
	}

	// Message 1: tool_result removed → placeholder.
	msg1 := gjson.GetBytes(out, "messages.1.content").Array()
	if len(msg1) != 1 || !strings.Contains(msg1[0].Get("text").String(), "omitted tool call") {
		t.Fatalf("msg1 tool_result not replaced with placeholder: %s", gjson.GetBytes(out, "messages.1.content").Raw)
	}

	// Message 2: text kept, tool_use_2 removed (no placeholder because text remains).
	msg2 := gjson.GetBytes(out, "messages.2.content").Array()
	if len(msg2) != 1 {
		t.Fatalf("msg2.content len = %d, want 1 (only text kept)", len(msg2))
	}
	if msg2[0].Get("type").String() != "text" {
		t.Fatalf("msg2[0].type = %q, want text", msg2[0].Get("type").String())
	}
}

// TestRemapToolNames_IdempotentOnTitleCaseNames: if a tool is already named
// Read/Write/Bash etc., do not add it to the remap table (no-op).
func TestRemapToolNames_IdempotentOnTitleCaseNames(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Read"},{"name":"Bash"},{"name":"WebFetch"}],"messages":[]}`)
	out, remap := RemapToolNames(body)

	// Tool names unchanged.
	for i, want := range []string{"Read", "Bash", "WebFetch"} {
		if got := gjson.GetBytes(out, "tools."+itoa(i)+".name").String(); got != want {
			t.Fatalf("tools[%d].name = %q, want %q (no change)", i, got, want)
		}
	}
	if len(remap) != 0 {
		t.Fatalf("expected empty remap on TitleCase input, got %+v", remap)
	}
}

func itoa(i int) string {
	switch i {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return "2"
	}
	return ""
}

// TestScrubThirdPartyBody_ReturnsRemapTable sanity checks the top-level
// orchestrator returns the rename map from the RemapToolNames pass.
func TestScrubThirdPartyBody_ReturnsRemapTable(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read"},{"name":"exec"}],"messages":[]}`)
	_, remap := ScrubThirdPartyBody(body)
	if remap["read"] != "Read" || remap["exec"] != "Bash" {
		t.Fatalf("remap table missing entries: %+v", remap)
	}
}

// TestScrubThirdPartyBody_FullIntegration feeds a realistic OpenClaw-shaped
// body through the orchestrator and asserts that every fingerprint the
// Anthropic detector looks for is neutralized:
//   - "OpenClaw" substring → replaced with "O<ZWSP>penClaw"
//   - "[System Instructions]" → removed
//   - lowercase tool names → renamed
//   - music_generate → dropped (tools + tool_use)
//   - <env> ... </env> XML → stripped
func TestScrubThirdPartyBody_FullIntegration(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-6",
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"[System Instructions]\nYou are a personal assistant running inside OpenClaw.\n## Tooling\n- read: Read file contents\n- write: Create or overwrite files\n- exec: Run shell commands\n- music_generate: Generate music\n\n<env>cwd=/root/.openclaw/workspace\nOS=Linux</env>Proceed with the task."}
			]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"music_generate","input":{"prompt":"ambient"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"song.mp3"}
			]}
		],
		"tools":[
			{"name":"read","description":"read files"},
			{"name":"write","description":"write files"},
			{"name":"music_generate","description":"generate music"}
		]
	}`)

	out, remap := ScrubThirdPartyBody(body)
	s := string(out)

	// 1. No raw "OpenClaw" substring.
	if strings.Contains(s, "OpenClaw") {
		t.Errorf("raw OpenClaw substring survived: %s", s)
	}
	// The obfuscated form must be present instead.
	if !strings.Contains(s, "O"+zeroWidthSpace+"penClaw") {
		t.Errorf("obfuscated O<ZWSP>penClaw not found: %s", s)
	}

	// 2. No [System Instructions] marker.
	if strings.Contains(s, "[System Instructions]") {
		t.Errorf("[System Instructions] marker survived: %s", s)
	}

	// 3. No <env>...</env>.
	if strings.Contains(s, "<env>") || strings.Contains(s, "</env>") {
		t.Errorf("env XML survived: %s", s)
	}
	if strings.Contains(s, "/root/.openclaw/workspace") {
		// The env content should be gone along with the tags.
		t.Errorf("env payload survived: %s", s)
	}

	// 4. No `## Tooling` block.
	if strings.Contains(s, "## Tooling") {
		t.Errorf("## Tooling section survived: %s", s)
	}

	// 5. tools[] renamed and music_generate dropped.
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Errorf("tools count = %d, want 2 (read+write kept, music_generate dropped)", len(tools))
	}
	gotNames := []string{tools[0].Get("name").String(), tools[1].Get("name").String()}
	wantContains := map[string]bool{"Read": true, "Write": true}
	for _, n := range gotNames {
		if !wantContains[n] {
			t.Errorf("unexpected tool name after scrub: %q (have %v)", n, gotNames)
		}
	}

	// 6. music_generate tool_use/tool_result replaced with placeholder.
	if gjson.GetBytes(out, `messages.1.content.0.type`).String() != "text" {
		t.Errorf("msg1 placeholder missing: %s", gjson.GetBytes(out, "messages.1.content").Raw)
	}

	// 7. Remap table surfaced.
	if remap["read"] != "Read" || remap["write"] != "Write" {
		t.Errorf("remap table missing entries: %+v", remap)
	}
}
