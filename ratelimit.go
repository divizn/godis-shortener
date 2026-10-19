package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const window = time.Hour

// allow counts a request from ip in the current window. The key expires when the window ends, so its TTL is the Retry-After
func (a *app) allow(ctx context.Context, kind, ip string) (ok bool, retryAfter time.Duration, err error) {
	now := a.now()
	start := now.Truncate(window)
	key := fmt.Sprintf("ratelimit/%s/%s/%d", kind, ip, start.Unix())
	until := start.Add(window).Sub(now)
	n, err := a.kv.Incr(ctx, key, 1)
	if err != nil {
		return false, 0, fmt.Errorf("count %s: %w", key, err)
	}
	if n == 1 {
		if _, err := a.kv.Expire(ctx, key, until); err != nil {
			return false, 0, fmt.Errorf("expire %s: %w", key, err)
		}
	}
	if n <= int64(a.cfg.createLimit) {
		return true, 0, nil
	}
	ttl, found, err := a.kv.TTL(ctx, key)
	if err != nil {
		return false, 0, fmt.Errorf("ttl %s: %w", key, err)
	}
	if !found || ttl <= 0 || ttl > until {
		ttl = until
	}
	return false, ttl, nil
}

// clientIP is the address requests are counted against: an IPv6 client by its /64, since one host usually holds the whole prefix.
// Behind a trusted proxy it is the last X-Forwarded-For hop, the one the proxy added; earlier hops come from the client.
func (a *app) clientIP(r *http.Request) string {
	if a.cfg.trustProxy {
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if ip, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
			return limitKey(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return limitKey(ip)
	}
	return "unknown"
}

func limitKey(ip netip.Addr) string {
	ip = ip.Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}
