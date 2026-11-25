package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type routeKind int

const (
	restRoute routeKind = iota
	fileRoute
	mcpRoute
)

type accessGuard struct {
	bearer   []byte
	port     string
	loopback bool
	proxies  []netip.Prefix
	limiter  rateLimiter
}

func newGuard(opts Options) *accessGuard {
	guard := &accessGuard{
		proxies: opts.TrustedProxies,
		limiter: rateLimiter{hits: make(map[string][]time.Time)},
	}
	if host, port, err := net.SplitHostPort(opts.Addr); err == nil {
		guard.port = port
		guard.loopback = strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
	}
	if opts.Token != "" {
		guard.bearer = []byte("Bearer " + opts.Token)
	}
	return guard
}

func (g *accessGuard) wrap(next http.Handler, kind routeKind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retry := g.limiter.hit(clientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), g.proxies)); retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			if kind == mcpRoute {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			} else {
				writeRetryProblem(w, retry)
			}
			return
		}
		if len(g.bearer) != 0 {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), g.bearer) != 1 {
				if kind == mcpRoute {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				} else {
					w.Header().Set("WWW-Authenticate", `Bearer realm="aakb"`)
					writeProblem(w, http.StatusUnauthorized, "UNAUTHORIZED", "A valid bearer token is required.")
				}
				return
			}
		} else if kind == restRoute && !g.allowTokenless(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP names the client for rate limiting. When the connection comes
// from a trusted proxy, it is the last X-Forwarded-For address that is not
// itself a trusted proxy. Otherwise it is the connection's own address.
func clientIP(remoteAddr string, forwarded []string, proxies []netip.Prefix) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr
	}
	if !trusted(ip, proxies) {
		return ip
	}
	hops := strings.Split(strings.Join(forwarded, ","), ",")
	for _, hop := range slices.Backward(hops) {
		hop = strings.TrimSpace(hop)
		if _, err := netip.ParseAddr(hop); err != nil {
			break
		}
		if !trusted(hop, proxies) {
			return hop
		}
	}
	return ip
}

func trusted(ip string, proxies []netip.Prefix) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range proxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (g *accessGuard) allowTokenless(w http.ResponseWriter, r *http.Request) bool {
	if !g.loopback {
		writeProblem(w, http.StatusForbidden, "TOKEN_REQUIRED", "Set AAKB_HTTP_TOKEN to use this resource on this listener.")
		return false
	}
	if !g.validHost(r.Host) {
		writeProblem(w, http.StatusForbidden, "BAD_HOST", "Use localhost or a loopback IP with the server's port.")
		return false
	}
	return true
}

func (g *accessGuard) validHost(authority string) bool {
	return strings.EqualFold(authority, net.JoinHostPort("localhost", g.port)) ||
		authority == net.JoinHostPort("127.0.0.1", g.port) ||
		authority == net.JoinHostPort("::1", g.port)
}

const requestsPerMinute = 120

type rateLimiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time
	nextSweep time.Time
}

// hit records a permitted request or returns the retry delay in whole seconds.
func (l *rateLimiter) hit(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	if !now.Before(l.nextSweep) {
		for key, hits := range l.hits {
			if !hits[len(hits)-1].After(cutoff) {
				delete(l.hits, key)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}

	hits := l.hits[ip]
	expired := 0
	for expired < len(hits) && !hits[expired].After(cutoff) {
		expired++
	}
	if len(hits)-expired >= requestsPerMinute {
		return int((hits[expired].Sub(cutoff) + time.Second - 1) / time.Second)
	}
	kept := copy(hits, hits[expired:])
	l.hits[ip] = append(hits[:kept], now)
	return 0
}
