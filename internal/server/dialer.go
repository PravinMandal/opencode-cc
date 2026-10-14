package server

import (
	"context"
	"net"
	"strings"
)

// Family-aware upstream dialing.
//
// Why this exists (probed live 2026-10-14): genuine opencode (Bun) resolves
// opencode.ai IPv4-first, while Go/Python/curl get IPv6-first from
// systemd-resolved. The IPv6 path egresses via a shared carrier range
// (2409:40c2::/32) whose free-tier budget was exhausted, while IPv4 stayed
// cool: same key/bytes/model/endpoint, 4/4 IPv4 edges returned 200 and 2/2
// IPv6 edges returned 429 FreeUsageLimitError, and Bun (v4) passed every
// trial while Go/Python/curl (v6) failed every trial. So:
//
//   - the shared upstream transport prefers IPv4 for opencode.ai (same fate
//     as genuine; custom non-Zen upstreams keep default dual-stack dialing),
//   - on a rate-limit rejection, doFreeTierPost replays once through a
//     v6-pinned alternate client (covers the inverted case where v4 trips
//     and v6 is cool), then surfaces whatever comes back.
//
// This is deliberately NOT TLS fingerprint mimicry: a byte-identical Bun
// ClientHello (JA3/JA4-verified) over IPv6 still 429s, so the discriminator
// is the path, not the handshake.

// isOpencodeAIHost reports whether host is opencode.ai or a subdomain.
func isOpencodeAIHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return h == "opencode.ai" || strings.HasSuffix(h, ".opencode.ai")
}

// orderAddrsForHost orders resolved addresses for dialing. Non-Zen hosts
// keep resolver order (untouched behavior). For opencode.ai, IPv4 goes
// first (genuine-client behavior); when forceV6 is set only IPv6 addresses
// are returned (failover path). Relative order within a family is kept
// (resolver round-robin still rotates edges).
func orderAddrsForHost(host string, addrs []net.IPAddr, forceV6 bool) []net.IPAddr {
	if !isOpencodeAIHost(host) {
		return addrs
	}
	var v4, v6 []net.IPAddr
	for _, a := range addrs {
		if a.IP == nil {
			continue
		}
		if a.IP.To4() != nil {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	if forceV6 {
		return v6
	}
	return append(v4, v6...)
}

// dialOpencodeAware returns an http.Transport DialContext func. base supplies
// timeouts/keepalive; forceV6 pins the failover transport to IPv6 ("" means
// prefer-IPv4). Non-Zen hosts always use base dialing unchanged.
func dialOpencodeAware(base *net.Dialer, forceV6 bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || !isOpencodeAIHost(host) {
			return base.DialContext(ctx, network, addr)
		}
		resolver := base.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		ordered := orderAddrsForHost(host, addrs, forceV6)
		if len(ordered) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		var firstErr error
		for _, a := range ordered {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			conn, err := base.DialContext(ctx, "tcp", net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil, firstErr
	}
}
