package server

import (
	"context"
	"net"
	"testing"
	"time"
)

func ip4(s string) net.IPAddr {
	return net.IPAddr{IP: net.ParseIP(s)}
}

func TestIsOpencodeAIHost(t *testing.T) {
	for _, h := range []string{"opencode.ai", "OPENCodE.AI", "zen.opencode.ai", "opencode.ai."} {
		if !isOpencodeAIHost(h) {
			t.Errorf("isOpencodeAIHost(%q) = false", h)
		}
	}
	for _, h := range []string{"", "example.com", "notopencode.ai.evil.com", "opencode.ai.evil.com", "127.0.0.1"} {
		if isOpencodeAIHost(h) {
			t.Errorf("isOpencodeAIHost(%q) = true", h)
		}
	}
}

func TestOrderAddrsForHost(t *testing.T) {
	addrs := []net.IPAddr{ip4("2606:4700:78::90:0:140"), ip4("172.65.90.20"), ip4("2606:4700:78::90:0:141"), ip4("172.65.90.21")}
	got := orderAddrsForHost("opencode.ai", addrs, false)
	want := []string{"172.65.90.20", "172.65.90.21", "2606:4700:78::90:0:140", "2606:4700:78::90:0:141"}
	for i := range want {
		if got[i].IP.String() != want[i] {
			t.Fatalf("prefer-v4 order = %v, want %v", got, want)
		}
	}
	got6 := orderAddrsForHost("opencode.ai", addrs, true)
	for _, a := range got6 {
		if a.IP.To4() != nil {
			t.Fatalf("forceV6 returned IPv4 %v", a)
		}
	}
	if len(got6) != 2 {
		t.Fatalf("forceV6 len = %d, want 2", len(got6))
	}
	// Non-Zen hosts keep resolver order untouched.
	same := orderAddrsForHost("custom.example", addrs, false)
	for i := range addrs {
		if same[i].IP.String() != addrs[i].IP.String() {
			t.Fatalf("non-Zen order changed: %v", same)
		}
	}
	// Empty input stays empty (triggers the no-address error path, not a panic).
	if out := orderAddrsForHost("opencode.ai", nil, false); len(out) != 0 {
		t.Fatalf("empty in = %v", out)
	}
}

func TestDialOpencodeAwareNonZenDelegates(t *testing.T) {
	// 127.0.0.1 is not opencode.ai: must dial normally (proves the wrapper
	// doesn't break custom upstreams or tests).
	base := &net.Dialer{Timeout: 5 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialOpencodeAware(base, false)(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("delegate dial: %v", err)
	}
	conn.Close()
	<-done
}
