package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Dynamic Zen console following.
//
// Why this exists: genuine opencode does NOT hardcode its Zen routes. At
// startup (and on credential changes) it fetches
// GET {zen-host}/console/api/v2/config and merges providers.opencode
// settings (baseURL) and headers (e.g. x-opencode-org-id) into every model
// request. Endpoint migrations and new required headers therefore apply to
// stock clients with no binary update — while a proxy with baked-in routes
// breaks until patched. This file gives opencode-cc the same property:
//
//   - consoleRouteFor: per-API-key cached console config (baseURL + headers
//     for the opencode provider), refreshed in the background and on demand.
//     Any fetch/parse failure returns nil and callers carry on exactly as
//     before, so a console outage can never break proxying.
//   - mergeConsoleHeaders: applies the configured provider headers to an
//     upstream request (never touching Authorization/Content-Type/UA).
//   - doFreeTierPost: drop-in for doUpstreamWithRetry on free-lane POSTs.
//     On a FreeTierError-signature 403 it rebuilds the identical request
//     against the alternate gateway (console baseURL, else the mirrored
//     /zen <-> /inference gateway) and tries once. Endpoint-specific gate
//     rules are therefore survived automatically.
//   - Gate watchdog: counts FreeTierError rejections; after a streak it logs
//     a loud runbook line (a genuinely novel rule still needs a human with
//     the capture→replay→bisect probes, but it is now detected in minutes,
//     not via user reports). Status is exposed for /healthz via
//     SetGateStatusFunc, mirroring api.SetInvalidateCache.

const (
	consoleConfigPath = "/console/api/v2/config"
	consoleFetchTTL   = 5 * time.Minute
	consoleFetchNeg   = time.Minute
	consoleTimeout    = 10 * time.Second

	// defaultOpenAIBase mirrors the providers.opencode settings.baseURL seen
	// in console configs (verified live 2026-10-14). Used only when the
	// console fetch fails but the primary upstream is opencode.ai.
	defaultOpenAIBase = "https://opencode.ai/inference/openai/v1"
	// defaultAnthropicBase mirrors the per-model settings.baseURL of the
	// anthropic-package models in the same configs. Used for the native
	// Messages lane under the same conditions.
	defaultAnthropicBase = "https://opencode.ai/inference/anthropic/v1"

	// gateAlertStreak is how many consecutive FreeTierError rejections
	// trigger the loud runbook log. Single 403s happen (flaky edge), a
	// streak means the gate moved.
	gateAlertStreak = 3
)

// consoleRoute is the live provider overlay for one upstream API key.
type consoleRoute struct {
	openAIBase    string
	anthropicBase string
	headers       map[string]string
	fetchedAt     time.Time
}

// consoleEntry caches one key's route (or its absence on failure).
type consoleEntry struct {
	route     *consoleRoute
	fetchedAt time.Time
	failed    bool
}

func (e *consoleEntry) fresh(now time.Time) bool {
	if e == nil {
		return false
	}
	ttl := consoleFetchTTL
	if e.failed || e.route == nil {
		ttl = consoleFetchNeg
	}
	return now.Sub(e.fetchedAt) < ttl
}

// consoleRouteCache maps API key -> console overlay. In-memory only.
type consoleRouteCache struct {
	mu    sync.Mutex
	byKey map[string]*consoleEntry
}

func newConsoleRouteCache() *consoleRouteCache {
	return &consoleRouteCache{byKey: map[string]*consoleEntry{}}
}

// consoleRouteFor returns the cached overlay for apiKey. upstreamBase scopes
// the console host (only opencode.ai is queried). With allowFetch it fetches
// on a stale/missing entry (failover and background refresh paths, where the
// latency is acceptable); the per-request header merge passes false so a
// console outage can never slow real traffic — it just runs unmerged until
// the background loop warms the cache. Never errors fatally: nil means
// "carry on with baked-in behavior".
func (s *Server) consoleRouteFor(apiKey, upstreamBase string, allowFetch bool) *consoleRoute {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	host := consoleHost(upstreamBase)
	if host == "" {
		return nil
	}
	now := time.Now()
	s.consoleRoutes.mu.Lock()
	e, ok := s.consoleRoutes.byKey[apiKey]
	if ok && e.fresh(now) {
		route := e.route
		s.consoleRoutes.mu.Unlock()
		return route
	}
	if !allowFetch {
		s.consoleRoutes.mu.Unlock()
		return nil
	}
	s.consoleRoutes.mu.Unlock()

	route := fetchConsoleRoute(host, apiKey)
	s.consoleRoutes.mu.Lock()
	s.consoleRoutes.byKey[apiKey] = &consoleEntry{route: route, fetchedAt: now, failed: route == nil}
	s.consoleRoutes.mu.Unlock()
	return route
}

// consoleHost returns "https://opencode.ai" for opencode.ai upstreams and ""
// otherwise (custom OpenAI-compatible upstreams have no console to follow).
func consoleHost(upstreamBase string) string {
	u, err := url.Parse(strings.TrimSpace(upstreamBase))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// fetchConsoleRoute GETs the console config and extracts the opencode
// provider overlay. Nil on any failure (network, non-200, bad JSON,
// missing provider) — callers fall back to baked-in behavior.
func fetchConsoleRoute(consoleHost, apiKey string) *consoleRoute {
	ctx, cancel := context.WithTimeout(context.Background(), consoleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, consoleHost+consoleConfigPath, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ocUA())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Key is dead/revoked: loud, exactly once per negative TTL window
		// (callers only reach here on cache miss).
		log.Printf("opencode-cc: console config rejected the upstream API key (HTTP %d); check the key in the web panel", resp.StatusCode)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	var cfg struct {
		Providers map[string]struct {
			Settings map[string]string `json:"settings"`
			Headers  map[string]string `json:"headers"`
			Models   map[string]struct {
				Package  string            `json:"package"`
				Settings map[string]string `json:"settings"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	prov, ok := cfg.Providers["opencode"]
	if !ok {
		return nil
	}
	route := &consoleRoute{
		openAIBase:    strings.TrimRight(prov.Settings["baseURL"], "/"),
		anthropicBase: "",
		headers:       map[string]string{},
		fetchedAt:     time.Now(),
	}
	for k, v := range prov.Headers {
		if strings.TrimSpace(v) == "" {
			continue
		}
		route.headers[k] = v
	}
	// Prefer a model-declared anthropic baseURL when present.
	for _, m := range prov.Models {
		if strings.Contains(m.Package, "anthropic") {
			if b := strings.TrimRight(m.Settings["baseURL"], "/"); b != "" {
				route.anthropicBase = b
				break
			}
		}
	}
	if route.openAIBase == "" {
		route.openAIBase = defaultOpenAIBase
	}
	if route.anthropicBase == "" {
		route.anthropicBase = defaultAnthropicBase
	}
	return route
}

// protectedConsoleHeaders are never overwritten by the console overlay:
// auth, framing, and our own client identity stay proxy-controlled.
func protectedConsoleHeader(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "authorization", "content-type", "content-length", "host", "user-agent", "accept", "accept-encoding":
		return true
	}
	return false
}

// mergeConsoleHeaders applies the console overlay headers for zenKey to
// upReq. No-op when no overlay is known (fetch failed): behavior is then
// exactly today's.
func (s *Server) mergeConsoleHeaders(upReq *http.Request, zenKey, upstreamBase string) {
	if upReq == nil || s == nil {
		return
	}
	route := s.consoleRouteFor(zenKey, upstreamBase, false)
	if route == nil {
		return
	}
	for k, v := range route.headers {
		if protectedConsoleHeader(k) || strings.TrimSpace(v) == "" {
			continue
		}
		upReq.Header.Set(k, v)
	}
}

// freeTierAltURL maps a primary upstream URL to the alternate gateway for
// the same protocol path, or "" when no alternate is known. Suffix is the
// request path ("/v1/responses", "/v1/chat/completions", "/v1/messages").
// Bases that already end in /v1 take the suffix minus its /v1 prefix
// (…/inference/openai/v1 + /responses); otherwise the full suffix is kept
// (…/zen + /v1/responses). Non-opencode.ai primaries never fail over.
func freeTierAltURL(primaryBase, suffix string, route *consoleRoute) string {
	primaryBase = strings.TrimRight(strings.TrimSpace(primaryBase), "/")
	if primaryBase == "" {
		return ""
	}
	u, err := url.Parse(primaryBase)
	if err != nil || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return ""
	}
	altBase := ""
	anthropic := strings.HasSuffix(suffix, "/messages")
	if route != nil {
		if anthropic && route.anthropicBase != "" {
			altBase = route.anthropicBase
		} else if !anthropic && route.openAIBase != "" {
			altBase = route.openAIBase
		}
	}
	if altBase == "" {
		if anthropic {
			altBase = defaultAnthropicBase
		} else {
			altBase = defaultOpenAIBase
		}
	}
	rel := suffix
	if strings.HasSuffix(strings.ToLower(altBase), "/v1") {
		rel = strings.TrimPrefix(suffix, "/v1")
		if rel == "" {
			rel = "/"
		}
	}
	alt := altBase + rel
	// Compare against the primary's own full URL (an inference-style
	// primary already ends in /v1, so it takes the trimmed suffix too —
	// otherwise the comparison double-counts /v1 and never matches).
	primaryFull := primaryBase + suffix
	if strings.HasSuffix(strings.ToLower(primaryBase), "/v1") {
		primaryFull = primaryBase + strings.TrimPrefix(suffix, "/v1")
	}
	if !strings.EqualFold(alt, primaryFull) {
		return alt
	}
	// Primary already equals the console route: fail the other way, back
	// to the /zen gateway (both directions are live Zen entrypoints).
	if strings.Contains(strings.ToLower(primaryBase), "/inference/") {
		return "https://opencode.ai/zen" + suffix
	}
	return ""
}

// isFreeTierRejection reports whether an upstream status+body is Zen's
// free-tier gate (as opposed to auth/rate/transport failures, which must
// never fail over or count toward the gate watchdog).
func isFreeTierRejection(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "free tier")
}

// doFreeTierPost is doUpstreamWithRetry plus free-lane failover: on a
// FreeTierError-signature 403 it replays the identical bytes against the
// alternate gateway once and returns that response (whatever its status),
// so endpoint-specific gate rules are survived without a rebuild. Non-403
// responses, transport errors, and paid-lane callers behave exactly like
// doUpstreamWithRetry. primaryBase/suffix locate the alternate gateway;
// pass suffix "" to disable failover (paid/native lanes).
func (s *Server) doFreeTierPost(client *http.Client, upReq *http.Request, upBody []byte, zenKey, primaryBase, suffix string) (*http.Response, error) {
	resp, err := doUpstreamWithRetry(client, upReq, upBody)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusForbidden || strings.TrimSpace(suffix) == "" {
		if resp.StatusCode < http.StatusBadRequest {
			s.noteGateSuccess()
		}
		return resp, nil
	}
	saved, ok := peekErrorBody(resp)
	if !ok || !isFreeTierRejection(resp.StatusCode, saved) {
		restoreErrorBody(resp, saved)
		return resp, nil
	}
	s.noteGateRejection()
	alt := freeTierAltURL(primaryBase, suffix, s.consoleRouteFor(zenKey, primaryBase, true))
	if alt == "" {
		restoreErrorBody(resp, saved)
		return resp, nil
	}
	log.Printf("opencode-cc: free-tier 403 on %s, failing over to %s (same bytes, one attempt)",
		truncateErr(upReq.URL.String()), truncateErr(alt))
	altReq := cloneWithBody(upReq, upBody)
	altURL, perr := url.Parse(alt)
	if perr != nil {
		restoreErrorBody(resp, saved)
		return resp, nil
	}
	altReq.URL = altURL
	altReq.Host = ""
	resp.Body.Close()
	altResp, altErr := doUpstreamWithRetry(client, altReq, upBody)
	if altErr != nil || altResp == nil {
		// Failover transport failed: report the original gate rejection,
		// which is more truthful than a dial error.
		restoreErrorBody(resp, saved)
		return resp, nil
	}
	if altResp.StatusCode < http.StatusBadRequest {
		s.noteGateSuccess()
	}
	return altResp, nil
}

// ---- gate watchdog ----

// gateAlertStreak is documented above with the constant block.
func (s *Server) noteGateRejection() {
	if s == nil {
		return
	}
	s.gateMu.Lock()
	s.gateConsec++
	s.gateTotal++
	n := s.gateConsec
	last := s.gateLastAlert
	now := time.Now()
	fire := n == gateAlertStreak || (n > gateAlertStreak && now.Sub(last) > 15*time.Minute)
	if fire {
		s.gateLastAlert = now
	}
	s.gateMu.Unlock()
	if fire {
		log.Printf("opencode-cc: FREE-TIER GATE CHANGE? %d consecutive FreeTierError rejections from Zen. "+
			"Genuine opencode still works? compare its request (BUN_CONFIG_VERBOSE_FETCH=curl) against ours, "+
			"then capture→replay→bisect single fields. Total gate rejections: %d.", n, s.gateTotalCount())
	}
}

func (s *Server) noteGateSuccess() {
	if s == nil {
		return
	}
	s.gateMu.Lock()
	s.gateConsec = 0
	s.gateMu.Unlock()
}

func (s *Server) gateTotalCount() int64 {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	return s.gateTotal
}

// GateStatus returns (total rejections, current consecutive streak) for the
// /healthz endpoint. Wired via SetGateStatusFunc like SetInvalidateCache.
func (s *Server) GateStatus() (int64, int) {
	if s == nil {
		return 0, 0
	}
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	return s.gateTotal, s.gateConsec
}

// refreshConsoleRoutes refreshes the console overlay for every configured
// upstream key. Failures are silent here (a loud log would spam every 5
// minutes during an outage); the next real request reports problems, and
// key-revocation still logs once per fetch in fetchConsoleRoute.
func (s *Server) refreshConsoleRoutes() {
	if s == nil || s.cfg == nil {
		return
	}
	type pair struct{ base, key string }
	var targets []pair
	snap := s.cfg.Snapshot()
	for _, u := range snap.Upstreams {
		if u.Enabled && strings.TrimSpace(u.BaseURL) != "" && strings.TrimSpace(u.APIKey) != "" {
			targets = append(targets, pair{u.BaseURL, u.APIKey})
		}
	}
	if legacyKey := strings.TrimSpace(snap.ZenAPIKey); legacyKey != "" {
		targets = append(targets, pair{snap.UpstreamBase, legacyKey})
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.key] {
			continue
		}
		seen[t.key] = true
		before := ""
		if e, ok := s.consoleRoutes.byKey[t.key]; ok && e.route != nil {
			before = e.route.openAIBase
		}
		route := s.consoleRouteFor(t.key, t.base, true)
		if route == nil {
			continue
		}
		// Log endpoint migrations (and the first successful load): a
		// changed baseURL is exactly the drift this layer exists to track.
		if before == "" {
			log.Printf("opencode-cc: console overlay loaded: upstream=%s base=%s headers=%d",
				truncateErr(t.base), truncateErr(route.openAIBase), len(route.headers))
		} else if route.openAIBase != before {
			log.Printf("opencode-cc: console overlay CHANGED upstream base: %s -> %s (following automatically)",
				truncateErr(before), truncateErr(route.openAIBase))
		}
	}
}

// startConsoleRefresh launches the background console-overlay refresh loop.
// Shares Shutdown's done channel with the keep-alive pinger.
func (s *Server) startConsoleRefresh() {
	if s == nil || s.cfg == nil {
		return
	}
	s.consoleRefreshOnce.Do(func() {
		go func() {
			s.refreshConsoleRoutes() // warm the cache at startup
			ticker := time.NewTicker(consoleFetchTTL)
			defer ticker.Stop()
			for {
				select {
				case <-s.done:
					return
				case <-ticker.C:
					s.refreshConsoleRoutes()
				}
			}
		}()
	})
}
