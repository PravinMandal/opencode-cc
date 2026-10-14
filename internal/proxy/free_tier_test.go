package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsFreeModel(t *testing.T) {
	for _, model := range []string{"big-pickle", "muse-spark-1.3-contributor-free", "nemotron-3.5-lightning-free", "x-preview-f-free"} {
		if !IsFreeModel(model) {
			t.Errorf("IsFreeModel(%q) = false, want true", model)
		}
	}
	for _, model := range []string{"gpt-5.5", "claude-sonnet", "muse-spark-1.3"} {
		if IsFreeModel(model) {
			t.Errorf("IsFreeModel(%q) = true, want false", model)
		}
	}
}

func TestPrepareFreeChatBodyAddsCanonicalGateShape(t *testing.T) {
	body := []byte(`{"model":"big-pickle","stream":false,"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
	out, err := PrepareFreeChatBody(body)
	if err != nil {
		t.Fatalf("PrepareFreeChatBody: %v", err)
	}
	var payload struct {
		Stream        bool            `json:"stream"`
		ToolChoice    any             `json:"tool_choice"`
		StreamOptions map[string]bool `json:"stream_options"`
		Tools         []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !payload.Stream || !payload.StreamOptions["include_usage"] {
		t.Fatalf("gate fields = stream:%v options:%v", payload.Stream, payload.StreamOptions)
	}
	// Oct 2026 gate: tool_choice must be ABSENT on the chat lane (stock
	// opencode omits it; sending "auto" 403s).
	if payload.ToolChoice != nil {
		t.Fatalf("tool_choice = %v, want absent on free chat lane", payload.ToolChoice)
	}
	seen := map[string]bool{}
	for _, tool := range payload.Tools {
		seen[tool.Function.Name] = true
	}
	for _, name := range []string{"Read", "shell", "read"} {
		if !seen[name] {
			t.Errorf("missing tool %q; tools=%v", name, seen)
		}
	}
}

func TestPrepareFreeResponsesBodyAddsGateTools(t *testing.T) {
	out, err := PrepareFreeResponsesBody([]byte(`{"model":"muse-spark-1.3-contributor-free","stream":false}`))
	if err != nil {
		t.Fatalf("PrepareFreeResponsesBody: %v", err)
	}
	if !strings.Contains(string(out), `"name":"shell"`) || !strings.Contains(string(out), `"name":"read"`) {
		t.Fatalf("response gate tools missing: %s", out)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("response stream was not forced: %s", out)
	}
	// Oct 2026 gate: marker text is blocklisted, compat tools must carry
	// stock opencode definitions.
	if strings.Contains(string(out), "compatibility marker") {
		t.Fatalf("marker text present; Zen rejects it: %s", out)
	}
	if !strings.Contains(string(out), "Execute a shell command") {
		t.Fatalf("stock shell description missing: %s", out)
	}
	if !strings.Contains(string(out), `"strict":false`) {
		t.Fatalf("stock strict:false missing: %s", out)
	}
}
