package server

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

const (
	// fallbackOCVersion is used only when no installed opencode binary reports
	// a stable version (missing binary, probe failure, or a prerelease-only
	// install). It is a known-real, known-accepted release: Zen's free-tier
	// gate validates the version value (probed 2026-10-14 — a real but
	// parallel-line 1.18.35 UA 403s while 2.0.18 200s), so the fallback must
	// name a release that exists on the v2 line, not a guessed number.
	fallbackOCVersion = "2.0.18"
	// The current OpenCode installation format is
	// opencode/<channel>/<version>/<client>. Zen's free-tier gate validates
	// this shape, so do not append the old AI SDK suffix here.
	zenUAChannel = "latest"
	zenUAClient  = "cli"
)

var (
	ocVersionOnce sync.Once
	ocVersion     string
	versionRe     = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)
)

// ocUA returns the current stock OpenCode installation User-Agent, for
// example "opencode/latest/2.0.12/cli". Zen's free-tier gate validates the
// channel/version/client shape as well as the semver, so this must not use
// the older "opencode/<version> ai-sdk/..." form.
func ocUA() string {
	ocVersionOnce.Do(func() {
		ocVersion = detectOCVersion()
	})
	return "opencode/" + zenUAChannel + "/" + ocVersion + "/" + zenUAClient
}

// parseOCVersion extracts the first semver from raw "--version" output:
// "opencode v2.0.5" -> "2.0.5",
// "opencode2 v0.0.0-next-17444" -> "0.0.0-next-17444". Empty when none found.
func parseOCVersion(out string) string {
	return versionRe.FindString(out)
}

// isStableOCVersion reports whether v is a stable opencode release, usable
// in the upstream User-Agent. Preview/dev builds (0.0.0, -next, any
// prerelease tag) are rejected: Zen's free-tier gate does not accept them.
func isStableOCVersion(v string) bool {
	if v == "" || strings.HasPrefix(v, "0.0.0") || strings.Contains(v, "next") {
		return false
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return false
	}
	return true
}

// compareSemver compares two stable X.Y.Z versions numerically.
// Returns -1/0/+1. Non-conforming inputs compare as equal (0) so callers
// treat them as unranked rather than crashing.
func compareSemver(a, b string) int {
	parse := func(v string) ([3]int, bool) {
		var out [3]int
		rest := v
		for i := 0; i < 3; i++ {
			n := 0
			digits := 0
			for len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
				n = n*10 + int(rest[0]-'0')
				rest = rest[1:]
				digits++
			}
			if digits == 0 {
				return out, false
			}
			out[i] = n
			if i < 2 {
				if len(rest) == 0 || rest[0] != '.' {
					return out, false
				}
				rest = rest[1:]
			}
		}
		return out, true
	}
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return 0
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// detectOCVersion probes every known opencode binary location and uses the
// NEWEST stable release found — never the first hit. Rationale (2026-10-14):
// version managers can shadow the real install with a parallel release line
// (e.g. a mise shim reporting v1.18.35 while ~/.opencode/bin carries
// v2.0.18), and Zen's free-tier gate validates the version VALUE, rejecting
// the older line. Newest-wins tracks user upgrades with no code change.
// Preview/dev builds are skipped; fallbackOCVersion is returned when no
// stable version is found anywhere.
func detectOCVersion() string {
	candidates := [][]string{
		{"/usr/bin/opencode", "--version"},
		{"/usr/local/bin/opencode", "--version"},
		{"opencode1", "--version"},
		{"opencode", "--version"},
	}
	// Explicit home installs: systemd services and other restricted-PATH
	// environments may not resolve bare "opencode" even though it exists.
	// The stock installer binary lives in ~/.opencode/bin.
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates,
			[]string{home + "/.opencode/bin/opencode", "--version"},
			[]string{home + "/.local/bin/opencode", "--version"},
		)
	}
	best := ""
	for _, c := range candidates {
		out, err := exec.Command(c[0], c[1:]...).Output()
		if err != nil {
			continue
		}
		if v := parseOCVersion(strings.TrimSpace(string(out))); isStableOCVersion(v) {
			if best == "" || compareSemver(v, best) > 0 {
				best = v
			}
		}
	}
	if best == "" {
		return fallbackOCVersion
	}
	return best
}
