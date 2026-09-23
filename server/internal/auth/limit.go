package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter bounds attempts to sign in or enrol, each of which spends a call to
// GitHub with a credential the caller chose. GitHub meets a run of bad ones by
// refusing this server's address for a while, which takes every colleague's
// sign-in and enrolment down with it. Attempts are counted per client address,
// so no one caller can use up the cap on all of them, which stays as a
// backstop against a caller with many addresses. Its zero value is ready.
type Limiter struct {
	mu     sync.Mutex
	start  time.Time
	total  int
	byAddr map[string]int
}

const (
	// perAddress is a minute's attempts from one address: enough for an
	// office behind one address to enrol together, not for a script.
	perAddress = 10
	// perMinute is a minute's attempts from every address together. A whole
	// team installing at once is well inside it.
	perMinute = 30
)

// Allow reports whether one more attempt from r's client fits this minute,
// counting it if so.
func (l *Limiter) Allow(r *http.Request) bool { return l.allow(clientAddr(r), time.Now()) }

func (l *Limiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= time.Minute {
		l.start, l.total, l.byAddr = now, 0, map[string]int{}
	}
	if l.byAddr[addr] >= perAddress || l.total >= perMinute {
		return false
	}
	l.byAddr[addr]++
	l.total++
	return true
}

// clientAddr is the address a request came from: the rightmost
// X-Forwarded-For entry when there is one, which is the address Caddy saw
// (it replaces whatever a client sent), and otherwise the connection's. An
// IPv6 client counts by its /64, which one host is typically given whole.
func clientAddr(r *http.Request) string {
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if last = strings.TrimSpace(last); last != "" {
			addr = last
		}
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		ap, perr := netip.ParseAddrPort(addr)
		if perr != nil {
			return addr
		}
		ip = ap.Addr()
	}
	if ip = ip.Unmap(); ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}
