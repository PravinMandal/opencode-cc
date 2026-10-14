package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// x-opencode-* identity headers, byte-identical to what the official opencode
// client sends on every Zen LLM request (see opencode session/llm.ts):
//
//	x-opencode-project  hex project id
//	x-opencode-session  ses_<26 alnum> session id (STICKY ROUTING + cache scope)
//	x-opencode-request  msg_<26 alnum> per-request id
//	x-opencode-client   client name ("cli" for stock terminal use,
//	                    "acp" for ACP mode; OPENCODE_CLIENT env overrides)
//
// The session header is load-bearing: Zen uses it as the sticky routing id
// for provider pinning and per-session cache scoping (falling back to the
// coarse workspace/IP when absent), and requests missing it may error after
// 2026-09-06. Zen strips these headers before forwarding to providers, so
// forwarding client values is safe and intended.
//
// Since ~2026-10-13 Zen additionally validates the session VALUE on the
// free-tier lane (FreeTierError "can only be used from within OpenCode" when
// it looks spoofed). Probed 2026-10-13/14 against the live gateway: the
// session must have the genuine opencode Identifier shape ses_<12 hex><14
// base62> where the 12-hex prefix is the DESCENDING timestamp
// (~(ts_ms*0x1000+ctr), schema/identifier.ts descending()). Fully random,
// all-hex (sha256), and ascending-timestamp sessions are rejected. Unknown
// sessions must also be FRESH (2h-old passes, ~3.3h-old is rejected as of
// 2026-10-14; previously-succeeded sessions appear exempt). Since 2026-10-14
// Zen also rejects any request carrying x-opencode-request (which stock
// opencode never sends — presence check) and any request whose shell/read
// tools are missing or carry spoof marker text (genuine-shaped shell+read
// required). The other session-carrying headers (x-session-affinity,
// x-session-id), prompt_cache_key, reasoning effort, and TLS stack were all
// probed and are NOT validated. Every synthesized value below reuses
// opencode's exact constructions; only the randomness sources differ
// (hash-derived for stickiness).
//
// This proxy must be indistinguishable from stock opencode upstream: it never
// sends any opencode-cc fingerprint in these headers.
const (
	zenHeaderProject = "x-opencode-project"
	zenHeaderSession = "x-opencode-session"
	// zenHeaderRequest is intentionally never sent upstream (see
	// setZenSessionHeaders): stock opencode omits it and Zen rejects any
	// request carrying it. Kept as a named constant for documentation.
	zenHeaderRequest   = "x-opencode-request"
	zenHeaderClient    = "x-opencode-client"
	zenHeaderAffinity  = "x-session-affinity"
	zenHeaderSessionID = "x-session-id"

	// zenClientID is the exact stock-terminal value (Flag.OPENCODE_CLIENT
	// defaults to "cli"), used when the downstream client sent none.
	zenClientID = "cli"
)

var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// hexOnlySessionPattern matches ses_ + 26 lowercase hex chars. No genuine
// opencode client can emit these (the 14-char tail is crypto-random base62,
// so all-hex has probability (16/62)^14): they are always a hash-derived
// spoof, and Zen's free-tier gate rejects them outright. They must be
// re-synthesized, never forwarded.
var hexOnlySessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{26}$`)

// sessionBucketMillis buckets synthesized session timestamps. Zen gates
// unknown sessions on prefix recency: 2h-old passes, ~3.3h-old is rejected
// (probed live 2026-10-14; previously-succeeded sessions appear exempt).
// A 30min bucket keeps every emitted session far inside the acceptance
// window while staying stable long enough for sticky routing and provider
// prompt-cache to work.
const sessionBucketMillis = int64(30 * 60 * 1000)

// sessionBucketStart returns the aligned bucket start (millis) for ts.
func sessionBucketStart(tsMillis int64) int64 {
	return tsMillis - (tsMillis % sessionBucketMillis)
}

// descendingSessionPrefix returns the 12-hex opencode Identifier prefix for
// a millisecond timestamp: the low 48 bits of ~(ts*0x1000+counter),
// big-endian hex. This replicates schema/identifier.ts descending() exactly
// (JS BigInt ~ is infinite-precision NOT; masking the Go uint64 NOT to 48
// bits yields the same low bytes).
func descendingSessionPrefix(tsMillis int64, counter uint64) string {
	current := (uint64(tsMillis)<<12 | (counter & 0xfff)) & 0xffffffffffff
	inv := ^current & 0xffffffffffff
	var b [6]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(inv >> (40 - 8*i))
	}
	return hex.EncodeToString(b[:])
}

// canonicalSessionFromHash builds a genuine-shaped session id: descending
// timestamp prefix for tsMillis plus a 14-char base62 tail drawn from sum.
// Deterministic in sum, so the same sticky key yields the same session
// within a time bucket (stable routing), while the shape is one Zen's
// free-tier gate accepts.
func canonicalSessionFromHash(sum [32]byte, tsMillis int64) string {
	// Small counter like a genuine client (per-process counter starting at
	// 1): keeps the low prefix bits in the ~0xFFF range stock opencode emits.
	counter := uint64(sum[31]%16) + 1
	var tail [14]byte
	for i := range tail {
		tail[i] = synthIDAlphabet[int(sum[i])%len(synthIDAlphabet)]
	}
	return "ses_" + descendingSessionPrefix(tsMillis, counter) + string(tail[:])
}

// maxDownstreamSessionAgeMs bounds how old a forwarded downstream session
// may be. Unknown sessions older than ~2h are rejected (probed live
// 2026-10-14); anything older is re-synthesized deterministically (same
// stickiness, guaranteed-fresh timestamp) rather than risk a FreeTierError
// on a stale conversation id. The 1h threshold keeps a 2x margin under the
// narrowest observed window.
const maxDownstreamSessionAgeMs = int64(60 * 60 * 1000)

// sessionPrefixAgeMs decodes the 12-hex descending prefix back to an age in
// milliseconds, reduced mod 2^36 (the prefix only carries ts mod 2^36, which
// wraps every ~2.2 years — still unambiguous against an hour-scale threshold).
func sessionPrefixAgeMs(prefix string, nowMs int64) (uint64, bool) {
	if len(prefix) != 12 {
		return 0, false
	}
	var v uint64
	for i := 0; i < 12; i += 2 {
		var b uint64
		for j := 0; j < 2; j++ {
			c := prefix[i+j]
			var d uint64
			switch {
			case c >= '0' && c <= '9':
				d = uint64(c - '0')
			case c >= 'a' && c <= 'f':
				d = uint64(c-'a') + 10
			default:
				return 0, false
			}
			b = b*16 + d
		}
		v = v*256 + b
	}
	const mask36 = uint64(0xfffffffff)
	recMs36 := ((^v & 0xffffffffffff) >> 12) & mask36
	// NOTE: parentheses are load-bearing — in Go, & binds tighter than -.
	return (((uint64(nowMs) & mask36) - recMs36) & mask36), true
}

// isForwardableSession reports whether a downstream session id may be sent
// upstream untouched: canonical shape, not an all-hex spoof, and fresh
// enough for Zen's free-tier gate.
func isForwardableSession(session string, nowMs int64) bool {
	if !canonicalSessionPattern.MatchString(session) || hexOnlySessionPattern.MatchString(session) {
		return false
	}
	age, ok := sessionPrefixAgeMs(strings.TrimPrefix(session, "ses_")[:12], nowMs)
	if !ok || age > uint64(maxDownstreamSessionAgeMs) {
		return false
	}
	return true
}

// resolveUpstreamSession selects the session id for an upstream request. It
// is pure (no request needed) so handlers can stamp it into the upstream
// body BEFORE sending: Zen's free-tier gate requires prompt_cache_key to
// equal x-opencode-session (probed 2026-10-14 — mismatched pairs 403 while
// matched pairs 200 under otherwise identical bytes), exactly as stock
// opencode sends (its cache key IS the session affinity).
func resolveUpstreamSession(down http.Header, stickyKey string) string {
	session := strings.TrimSpace(down.Get(zenHeaderSession))
	if session == "" {
		session = strings.TrimSpace(down.Get(zenHeaderAffinity))
	}
	if session == "" {
		session = strings.TrimSpace(down.Get(zenHeaderSessionID))
	}
	if session == "" {
		session = strings.TrimSpace(down.Get("x-claude-code-session-id"))
	}
	if !isForwardableSession(session, time.Now().UnixMilli()) {
		session = synthSessionID(session + "|" + strings.TrimSpace(stickyKey))
	}
	if session == "" {
		session = fallbackSessionID()
	}
	return session
}

// setZenSessionHeadersWithSession sets the x-opencode-* identity headers
// from an explicitly resolved session (see resolveUpstreamSession). Use it
// whenever the handler also stamps the session into the upstream body, so
// header and prompt_cache_key are guaranteed identical.
func setZenSessionHeadersWithSession(upReq *http.Request, down http.Header, session string) {
	if upReq == nil {
		return
	}
	upReq.Header.Set(zenHeaderSession, session)
	// The current OpenCode request path also sends these two affinity forms;
	// keeping them equal avoids a second identity path in Zen's router.
	upReq.Header.Set(zenHeaderAffinity, session)
	upReq.Header.Set(zenHeaderSessionID, session)

	// NOTE (2026-10-14 probe): x-opencode-request (zenHeaderRequest) is
	// deliberately NEVER sent upstream, even when the downstream client
	// provided one. Stock opencode does not emit this header on Zen LLM
	// requests, and Zen's free-tier gate rejects any request carrying it
	// (presence check — even a genuine-shaped ascending value 403s, while
	// the identical request without it returns 200). Forwarding a
	// downstream value would fail the same way, so it is stripped.

	project := strings.TrimSpace(down.Get(zenHeaderProject))
	if project == "" {
		project = "global"
	}
	upReq.Header.Set(zenHeaderProject, project)

	client := strings.TrimSpace(down.Get(zenHeaderClient))
	if client == "" {
		client = zenClientID
	}
	upReq.Header.Set(zenHeaderClient, client)
}

// setZenSessionHeaders sets the x-opencode-* identity headers on an upstream
// Zen request. Downstream session IDs are forwarded when fresh and
// genuine-shaped; UUIDs, older ses_ formats, all-hex spoof shapes, and stale
// sessions are deterministically normalized so every client reaches the
// same sticky-routing lane with a gate-accepted value.
//
// Handlers that also carry a prompt_cache_key in the upstream body must use
// resolveUpstreamSession + stampPromptCacheKey + setZenSessionHeadersWithSession
// instead, so header and body key are identical (Zen rejects mismatched
// pairs on the free-tier lane).
func setZenSessionHeaders(upReq *http.Request, down http.Header, stickyKey string) {
	setZenSessionHeadersWithSession(upReq, down, resolveUpstreamSession(down, stickyKey))
}

// stampPromptCacheKey sets prompt_cache_key=session on an OpenAI Chat or
// Responses JSON body, keeping it identical to x-opencode-session the way
// stock opencode sends it. Non-object bodies pass through untouched.
func stampPromptCacheKey(upBody []byte, session string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(upBody, &payload); err != nil {
		return nil, fmt.Errorf("decode upstream body: %w", err)
	}
	if payload == nil {
		return upBody, nil
	}
	key, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("encode session: %w", err)
	}
	payload["prompt_cache_key"] = key
	return json.Marshal(payload)
}

// synthSessionID derives a stock-formatted session id deterministically from
// the sticky key: a genuine descending-timestamp prefix for the current 30m
// bucket plus a hash-derived base62 tail. Same sticky key maps to the same
// upstream session within a bucket (stable sticky routing across turns and
// restarts); the bucket rotation keeps the timestamp fresh for Zen's
// free-tier gate, which rejects hash-shaped (all-hex) sessions outright.
// See the package header comment for the 2026-10-13 probe results.
func synthSessionID(stickyKey string) string {
	if strings.TrimSpace(stickyKey) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(stickyKey))
	return canonicalSessionFromHash(sum, sessionBucketStart(time.Now().UnixMilli()))
}

// synthIDAlphabet is byte-identical to opencode's schema/identifier.ts
// charset: digits first. Both session tails and request ids map random bytes
// with byte%62 exactly like stock opencode (including its modulo bias).
const synthIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// fallbackSessionID is the session id used when neither the downstream
// client nor the sticky key yields one. It follows Zen's accepted shape: a
// genuine descending-timestamp prefix plus random base62 tail. The tail is
// process-stable (coherent routing); the prefix tracks the current 30m
// bucket so long-lived processes never go stale for the free-tier gate.
var (
	fallbackSessionOnce sync.Once
	fallbackSessionTail [14]byte
)

func fallbackSessionID() string {
	fallbackSessionOnce.Do(func() {
		var b [14]byte
		if _, err := rand.Read(b[:]); err != nil {
			for i := range b {
				b[i] = byte(i * 37)
			}
		}
		fallbackSessionTail = b
	})
	var tail [14]byte
	for i, v := range fallbackSessionTail {
		tail[i] = synthIDAlphabet[int(v)%len(synthIDAlphabet)]
	}
	return "ses_" + descendingSessionPrefix(sessionBucketStart(time.Now().UnixMilli()), 1) + string(tail[:])
}

// NOTE: there is deliberately no request-id synthesis here. Stock opencode
// does not send x-opencode-request on Zen LLM requests and Zen rejects any
// request carrying it (2026-10-14 probe), so the header is never emitted;
// see setZenSessionHeaders.
