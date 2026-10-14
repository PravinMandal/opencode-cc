package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServerWithConsole() *Server {
	return &Server{consoleRoutes: newConsoleRouteCache()}
}

func TestFetchConsoleRouteParsesOverlay(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		if r.URL.Path != "/console/api/v2/config" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"providers":{"opencode":{
			"settings":{"baseURL":"https://opencode.ai/inference/openai/v1","apiKey":"{env:X}"},
			"headers":{"x-opencode-org-id":"wrk_test123"},
			"models":{"m":{"package":"aisdk:@ai-sdk/anthropic","settings":{"baseURL":"https://opencode.ai/inference/anthropic/v1"}}}}}}`)
	}))
	defer srv.Close()

	route := fetchConsoleRoute(srv.URL, "sk-test")
	if route == nil {
		t.Fatal("nil route on 200 valid JSON")
	}
	if route.openAIBase != "https://opencode.ai/inference/openai/v1" {
		t.Errorf("openAIBase = %q", route.openAIBase)
	}
	if route.anthropicBase != "https://opencode.ai/inference/anthropic/v1" {
		t.Errorf("anthropicBase = %q", route.anthropicBase)
	}
	if route.headers["x-opencode-org-id"] != "wrk_test123" {
		t.Errorf("headers = %v", route.headers)
	}
	if gotAuth != "Bearer sk-test" || !strings.HasPrefix(gotUA, "opencode/") {
		t.Errorf("auth=%q ua=%q", gotAuth, gotUA)
	}
}

func TestFetchConsoleRouteFailuresNil(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	if fetchConsoleRoute(bad.URL, "sk-dead") != nil {
		t.Errorf("want nil on 401 (dead key)")
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	}))
	defer broken.Close()
	if fetchConsoleRoute(broken.URL, "k") != nil {
		t.Errorf("want nil on bad JSON")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"providers":{}}`)
	}))
	defer empty.Close()
	if fetchConsoleRoute(empty.URL, "k") != nil {
		t.Errorf("want nil when opencode provider missing")
	}
}

func TestFreeTierAltURL(t *testing.T) {
	route := &consoleRoute{openAIBase: "https://opencode.ai/inference/openai/v1", anthropicBase: "https://opencode.ai/inference/anthropic/v1"}
	cases := []struct {
		primary, suffix string
		route           *consoleRoute
		want            string
	}{
		{"https://opencode.ai/zen", "/v1/responses", route, "https://opencode.ai/inference/openai/v1/responses"},
		{"https://opencode.ai/zen/", "/v1/chat/completions", route, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/zen", "/v1/messages", route, "https://opencode.ai/inference/anthropic/v1/messages"},
		{"https://opencode.ai/zen", "/v1/responses", nil, "https://opencode.ai/inference/openai/v1/responses"},
		{"https://opencode.ai/inference/openai/v1", "/v1/responses", route, "https://opencode.ai/zen/v1/responses"},
		{"https://custom.example/v1", "/v1/responses", route, ""},
		{"", "/v1/responses", route, ""},
		{"https://opencode.ai/zen", "/v1/responses", &consoleRoute{openAIBase: "https://opencode.ai/zen"}, ""},
	}
	for _, c := range cases {
		if got := freeTierAltURL(c.primary, c.suffix, c.route); got != c.want {
			t.Errorf("alt(%q, %q) = %q, want %q", c.primary, c.suffix, got, c.want)
		}
	}
}

func TestIsFreeTierRejection(t *testing.T) {
	if !isFreeTierRejection(403, []byte(`{"error":{"message":"OpenCode's free tier can only be used"}}`)) {
		t.Errorf("genuine gate body not detected")
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{401, "free tier"}, {429, "free tier"}, {500, "free tier"},
		{403, "invalid api key"}, {403, ""}, {200, "free tier"},
	} {
		if isFreeTierRejection(tc.status, []byte(tc.body)) {
			t.Errorf("false positive: %d %q", tc.status, tc.body)
		}
	}
}

func TestMergeConsoleHeaders(t *testing.T) {
	s := testServerWithConsole()
	// Non-opencode.ai upstreams never consult the console: headers pass
	// through untouched and auth is preserved.
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer k")
	s.mergeConsoleHeaders(req, "k", "https://custom.example/v1")
	if got := req.Header.Get("x-opencode-org-id"); got != "" {
		t.Errorf("custom upstream got console headers: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer k" {
		t.Errorf("authorization clobbered: %q", got)
	}
	// Overlay application itself is covered by fetch tests + the failover
	// test's pre-seeded cache; this asserts the protected-header list.
	if !protectedConsoleHeader("Authorization") || !protectedConsoleHeader("User-Agent") ||
		!protectedConsoleHeader("Content-Type") || protectedConsoleHeader("x-opencode-org-id") {
		t.Errorf("protected header classification wrong")
	}
}

func TestMergeConsoleHeadersAppliesOverlay(t *testing.T) {
	s := testServerWithConsole()
	// Pre-seeded fresh cache: no network happens (fetch only runs on miss).
	s.consoleRoutes.byKey["k"] = &consoleEntry{
		route: &consoleRoute{headers: map[string]string{
			"x-opencode-org-id": "wrk_1",
			"authorization":     "Bearer evil",
			"user-agent":        "evil",
		}, fetchedAt: time.Now()},
		fetchedAt: time.Now(),
	}
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("User-Agent", "opencode/latest/2.0.18/cli")
	s.mergeConsoleHeaders(req, "k", "https://opencode.ai/zen")
	if got := req.Header.Get("x-opencode-org-id"); got != "wrk_1" {
		t.Errorf("org-id = %q, want merged wrk_1", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer k" {
		t.Errorf("authorization = %q, must never be overwritten", got)
	}
	if got := req.Header.Get("User-Agent"); got != "opencode/latest/2.0.18/cli" {
		t.Errorf("user-agent = %q, must never be overwritten", got)
	}
}

func TestDoFreeTierPostFailover(t *testing.T) {
	altHits := 0
	alt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		altHits++
		if r.URL.Path != "/v1/responses" {
			t.Errorf("alt path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer alt.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	}))
	defer primary.Close()

	s := testServerWithConsole()
	// Pre-seed: alternate base is the mock alt server. freeTierAltURL keeps
	// the full suffix when the base lacks a /v1 tail.
	s.consoleRoutes.byKey["k"] = &consoleEntry{
		route:     &consoleRoute{openAIBase: alt.URL, fetchedAt: time.Now()},
		fetchedAt: time.Now(),
	}
	_ = primary
	upReq, _ := http.NewRequest(http.MethodPost, primary.URL+"/v1/responses", strings.NewReader(`{}`))
	resp, err := s.doFreeTierPost(http.DefaultClient, upReq, []byte(`{}`), "k", "https://opencode.ai/zen", "/v1/responses")
	if err != nil {
		t.Fatalf("failover err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want failover 200", resp.StatusCode)
	}
	if altHits != 1 {
		t.Errorf("alt hits = %d, want 1", altHits)
	}
	if total, consec := s.GateStatus(); total != 1 || consec != 0 {
		t.Errorf("watchdog total=%d consec=%d, want 1/0 (success resets streak)", total, consec)
	}
}

func TestDoFreeTierPostNoFailoverOnOtherErrors(t *testing.T) {
	s := testServerWithConsole()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"auth", 401, "invalid key"},
		{"rate", 429, "slow down"},
		{"other-403", 403, "quota exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altHits := 0
			alt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				altHits++
				w.WriteHeader(http.StatusOK)
			}))
			defer alt.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			s.consoleRoutes.byKey["k"] = &consoleEntry{
				route:     &consoleRoute{openAIBase: alt.URL, fetchedAt: time.Now()},
				fetchedAt: time.Now(),
			}
			upReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{}`))
			resp, err := s.doFreeTierPost(http.DefaultClient, upReq, []byte(`{}`), "k", "https://opencode.ai/zen", "/v1/responses")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || string(raw) != tc.body {
				t.Errorf("got %d %q, want passthrough %d %q", resp.StatusCode, raw, tc.status, tc.body)
			}
			if altHits != 0 {
				t.Errorf("failover fired on non-gate error")
			}
		})
	}
	if total, consec := s.GateStatus(); total != 0 || consec != 0 {
		t.Errorf("watchdog total=%d consec=%d, want 0/0", total, consec)
	}
}
