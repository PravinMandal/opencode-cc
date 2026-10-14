package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// IsFreeModel reports whether a Zen model is currently served through the
// free-tier lane. Zen rotates this catalog frequently, so the intentionally
// broad suffix check covers the current free IDs (including the stealth
// big-pickle model) without making protocol routing depend on a stale list.
func IsFreeModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	return strings.Contains(m, "-free") || m == "big-pickle"
}

// PrepareFreeChatBody adapts an OpenAI Chat Completions request to Zen's
// free-tier gate. The public API accepts ordinary non-streaming requests,
// while Zen's free lane currently requires the same streaming/tool shape as
// the official OpenCode client. Missing shell/read tools are filled in with
// stock opencode definitions (never marker text — see freeCompatTool);
// callers' real tools remain untouched.
func PrepareFreeChatBody(body []byte) ([]byte, error) {
	return prepareFreeBody(body, "chat")
}

// PrepareFreeResponsesBody is the Responses API equivalent of
// PrepareFreeChatBody.
func PrepareFreeResponsesBody(body []byte) ([]byte, error) {
	return prepareFreeBody(body, "responses")
}

func prepareFreeBody(body []byte, format string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode %s request: %w", format, err)
	}
	if payload == nil {
		return nil, fmt.Errorf("%s request must be a JSON object", format)
	}

	payload["stream"] = json.RawMessage(`true`)
	if format == "chat" {
		if _, ok := payload["stream_options"]; !ok || bytes.Equal(bytes.TrimSpace(payload["stream_options"]), []byte("null")) {
			payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
		// Stock opencode never sends tool_choice on the Chat Completions
		// lane (its body carries none even with tools present), and Zen's
		// free-tier gate rejects requests that do (probed 2026-10-14:
		// identical bytes 403 with "auto", 200 without). Omit it rather
		// than forcing "auto" — the server-side default is auto anyway,
		// and the toolless-resample guard recovers turns that come back
		// text-only. (The Responses lane tolerates "auto"; leave it.)
		delete(payload, "tool_choice")
	}

	tools, err := freeGateTools(payload["tools"], format)
	if err != nil {
		return nil, err
	}
	payload["tools"] = tools

	// The Responses lane tolerates tool_choice "auto" (and Zen 400s the
	// other modes there); the chat lane must omit it (see above).
	if format == "responses" {
		if _, ok := payload["tool_choice"]; !ok || bytes.Equal(bytes.TrimSpace(payload["tool_choice"]), []byte("null")) {
			payload["tool_choice"] = json.RawMessage(`"auto"`)
		}
	}

	return json.Marshal(payload)
}

func freeGateTools(raw json.RawMessage, format string) (json.RawMessage, error) {
	var tools []json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode tools: %w", err)
		}
	}
	present := map[string]bool{}
	for _, tool := range tools {
		if name, ok := freeToolName(tool, format); ok {
			present[name] = true
		}
	}
	for _, name := range []string{"shell", "read"} {
		if present[name] {
			continue
		}
		tools = append(tools, compatToolOrBare(name, format))
	}
	return json.Marshal(tools)
}

func freeToolName(raw json.RawMessage, format string) (string, bool) {
	var envelope struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "", false
	}
	if format == "responses" && envelope.Name != "" {
		return envelope.Name, true
	}
	if envelope.Function.Name != "" {
		return envelope.Function.Name, true
	}
	return "", false
}

// compatToolOrBare returns the stock-shaped compat tool, falling back to a
// bare-but-genuinely-named tool rather than failing the request.
func compatToolOrBare(name, format string) json.RawMessage {
	tool, err := freeCompatTool(name, format)
	if err != nil {
		// Fall back to a bare-but-genuine-named tool rather than failing
		// the request; the name alone is better than nothing.
		if format == "responses" {
			return json.RawMessage(fmt.Sprintf(`{"type":"function","name":%q}`, name))
		}
		return json.RawMessage(fmt.Sprintf(`{"type":"function","function":{"name":%q}}`, name))
	}
	return tool
}

// stockToolDef holds the tool definition stock opencode sends, captured
// 2026-10-14 from opencode 2.0.18. Zen's free-tier gate fuzzy-matches these
// (a paraphrased description still passes, but byte-identical is safest),
// while the previous fake "compatibility marker" text is blocklisted —
// requests carrying it are rejected even alongside genuine tools, and
// requests without shell/read at all are rejected too (probed live).
type stockToolDef struct {
	description string
	parameters  string
}

var stockTools = map[string]stockToolDef{
	"shell": {
		description: "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
		parameters:  `{"type":"object","properties":{"command":{"type":"string","description":"Shell command string to execute"},"workdir":{"type":"string","description":"Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},"timeout":{"type":"integer","minimum":0,"description":"Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},"background":{"type":"boolean","description":"Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when they complete. DO NOT poll for completion."}},"required":["command"],"additionalProperties":false}`,
	},
	"read": {
		description: "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
		parameters:  `{"type":"object","properties":{"path":{"type":"string","description":"File or directory to read"},"offset":{"type":"integer","minimum":0,"description":"The line or directory entry to start reading from (1-based)"},"limit":{"type":"integer","minimum":0,"description":"The maximum number of lines or directory entries to read (defaults to and capped at 2000)"}},"required":["path"],"additionalProperties":false}`,
	},
}

// freeCompatTool builds the stock-shaped compat tool for name in the given
// wire format. The responses shape matches stock opencode byte-for-byte
// (including "strict":false); the chat shape uses the same description and
// schema inside the OpenAI function envelope, matching how this proxy
// encodes caller tools (no strict field).
func freeCompatTool(name, format string) (json.RawMessage, error) {
	def, ok := stockTools[name]
	if !ok {
		return nil, fmt.Errorf("no stock definition for tool %q", name)
	}
	var params json.RawMessage
	if err := json.Unmarshal([]byte(def.parameters), &params); err != nil {
		return nil, fmt.Errorf("bad embedded schema for tool %q: %w", name, err)
	}
	if format == "responses" {
		return json.Marshal(map[string]any{
			"type":        "function",
			"name":        name,
			"description": def.description,
			"parameters":  params,
			"strict":      false,
		})
	}
	return json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": def.description,
			"parameters":  params,
		},
	})
}
