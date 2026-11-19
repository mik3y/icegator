package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/session"
)

// keyAlphabet leaves out 0, O, 1, I and L, which are easily confused when
// read aloud or copied by hand. Eight characters carry about 40 bits, which
// is why failed attempts are rate-limited.
const keyAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// NewKey generates a stream key in display form, e.g. "A7K3-9FQX".
func NewKey() string {
	b := make([]byte, 8)
	for i := range b {
		for {
			var r [1]byte
			rand.Read(r[:])
			// Reject the tail of the byte range so every character
			// is equally likely.
			if int(r[0]) < 256-256%len(keyAlphabet) {
				b[i] = keyAlphabet[int(r[0])%len(keyAlphabet)]
				break
			}
		}
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// NormalizeKey makes key comparison forgiving by ignoring case, hyphens
// and spaces.
func NormalizeKey(key string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(key))
}

// DJ is one entry in a locally held DJ list.
type DJ struct {
	Slug string
	Name string
	// Key is the DJ's stream key, or whatever stands in for it (see
	// Local.Digest).
	Key string
}

// Local checks DJs against a list of stream keys held by Icegator.
type Local struct {
	// Digest, if set, means the list holds a digest of each key and not
	// the key itself. It is applied to the normalised password a DJ
	// presents before comparing. This is how a list served as HMACs is
	// checked without Icegator ever holding a stream key.
	Digest func(normalizedKey string) string

	mu     sync.RWMutex
	bySlug map[string]DJ
	byKey  map[string]DJ
	limit  *Limiter
}

// NewLocal validates the list. Keys must be unique as well as slugs, because
// a DJ whose software insists on the username "source" is found by key alone.
func NewLocal(djs []DJ) (*Local, error) {
	l := &Local{limit: NewLimiter(5, 5*time.Minute)}
	return l, l.Replace(djs)
}

// Replace swaps in a new list. On error the old list stays in force.
func (l *Local) Replace(djs []DJ) error {
	bySlug, byKey := map[string]DJ{}, map[string]DJ{}
	for _, dj := range djs {
		key := dj.Key
		if l.Digest == nil {
			key = NormalizeKey(key)
		}
		slug := strings.ToLower(dj.Slug)
		switch {
		case slug == "" || slug == "source":
			return fmt.Errorf("dj %q: slug must be set and cannot be \"source\"", dj.Name)
		case len(key) < 8:
			return fmt.Errorf("dj %q: key must be at least 8 characters", dj.Slug)
		}
		if _, dup := bySlug[slug]; dup {
			return fmt.Errorf("dj %q: duplicate slug", dj.Slug)
		}
		if other, dup := byKey[key]; dup {
			return fmt.Errorf("djs %q and %q share a key", other.Slug, dj.Slug)
		}
		dj.Key = key
		bySlug[slug], byKey[key] = dj, dj
	}
	l.mu.Lock()
	l.bySlug, l.byKey = bySlug, byKey
	l.mu.Unlock()
	return nil
}

func (l *Local) Authenticate(_ context.Context, username, password, ip string) (session.Identity, error) {
	username, password = SplitShoutcast(username, password)
	slug := strings.ToLower(username)
	ipKey, userKey := "ip:"+ip, "user:"+slug
	if l.limit.Blocked(ipKey, userKey) {
		return session.Identity{}, ErrLimited
	}
	key := NormalizeKey(password)
	if l.Digest != nil {
		key = l.Digest(key)
	}

	l.mu.RLock()
	var dj DJ
	var ok bool
	var why string
	if slug == "" || slug == "source" {
		dj, ok = l.byKey[key]
		userKey = ipKey // nothing to lock but the address
		why = "no DJ has the stream key that was sent (no username was given, so the key alone had to match)"
	} else if dj, ok = l.bySlug[slug]; ok {
		ok = subtle.ConstantTimeCompare([]byte(dj.Key), []byte(key)) == 1
		why = "the stream key is not this DJ's"
	} else {
		why = fmt.Sprintf("no DJ with a stream key has the username %q (%d DJs are known)", slug, len(l.bySlug))
	}
	l.mu.RUnlock()
	if !ok {
		l.limit.Fail(ipKey, userKey)
		return session.Identity{}, fmt.Errorf("%w: %s", ErrDenied, why)
	}
	return session.Identity{Username: dj.Slug, Name: dj.Name}, nil
}
