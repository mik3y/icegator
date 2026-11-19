// Package auth decides whether a connecting DJ is let in, and who they are.
//
// Icegator holds the DJs and their stream keys, from its own config or
// from a provider such as StreamLover, and checks them itself. The station
// sees one account, which is Icegator's.
package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/session"
)

var (
	// ErrDenied means the credentials are wrong. Authenticators wrap it
	// with what exactly was wrong, for the operator's log. The DJ is told
	// only that the login failed.
	ErrDenied = errors.New("login refused")
	// ErrLimited means too many attempts or connections; try later.
	ErrLimited = errors.New("auth: too many attempts")
)

// Authenticator admits DJs.
type Authenticator interface {
	// Authenticate returns the DJ's identity or ErrDenied / ErrLimited.
	Authenticate(ctx context.Context, username, password, remoteIP string) (session.Identity, error)
}

// SplitShoutcast handles source clients that can only send a password and
// so pack both parts into it, as "user:pass" or "user,pass" with the
// username left at "source" (the convention Azuracast documents).
func SplitShoutcast(username, password string) (string, string) {
	if username != "" && username != "source" {
		return username, password
	}
	if i := strings.IndexAny(password, ":,"); i > 0 {
		return password[:i], password[i+1:]
	}
	return username, password
}

// RemoteIP strips the port from a network address.
func RemoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// Limiter locks a subject out after max failures within window.
type Limiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	failures map[string][]time.Time
	now      func() time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, failures: map[string][]time.Time{}, now: time.Now}
}

func (l *Limiter) recent(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	f := l.failures[key]
	for len(f) > 0 && f[0].Before(cutoff) {
		f = f[1:]
	}
	if len(f) == 0 {
		delete(l.failures, key)
	} else {
		l.failures[key] = f
	}
	return f
}

func (l *Limiter) Blocked(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if len(l.recent(k)) >= l.max {
			return true
		}
	}
	return false
}

func (l *Limiter) Fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		l.failures[k] = append(l.recent(k), l.now())
	}
}
