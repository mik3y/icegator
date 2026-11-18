package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"strings"
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
	Key  string
}

// Local checks DJs against a fixed list of stream keys.
type Local struct {
	bySlug map[string]DJ
	byKey  map[string]DJ
	limit  *Limiter
}

// NewLocal validates the list. Keys must be unique as well as slugs, because
// a DJ whose software insists on the username "source" is found by key alone.
func NewLocal(djs []DJ) (*Local, error) {
	l := &Local{bySlug: map[string]DJ{}, byKey: map[string]DJ{}, limit: NewLimiter(5, 5*time.Minute)}
	for _, dj := range djs {
		key := NormalizeKey(dj.Key)
		slug := strings.ToLower(dj.Slug)
		switch {
		case slug == "" || slug == "source":
			return nil, fmt.Errorf("dj %q: slug must be set and cannot be \"source\"", dj.Name)
		case len(key) < 8:
			return nil, fmt.Errorf("dj %q: key must be at least 8 characters", dj.Slug)
		}
		if _, dup := l.bySlug[slug]; dup {
			return nil, fmt.Errorf("dj %q: duplicate slug", dj.Slug)
		}
		if other, dup := l.byKey[key]; dup {
			return nil, fmt.Errorf("djs %q and %q share a key", other.Slug, dj.Slug)
		}
		dj.Key = key
		l.bySlug[slug], l.byKey[key] = dj, dj
	}
	return l, nil
}

func (l *Local) Authenticate(_ context.Context, username, password, ip string) (session.Identity, error) {
	username, password = SplitShoutcast(username, password)
	slug := strings.ToLower(username)
	ipKey, userKey := "ip:"+ip, "user:"+slug
	if l.limit.Blocked(ipKey, userKey) {
		return session.Identity{}, ErrLimited
	}
	key := NormalizeKey(password)

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
	if !ok {
		l.limit.Fail(ipKey, userKey)
		return session.Identity{}, fmt.Errorf("%w: %s", ErrDenied, why)
	}
	return session.Identity{Username: dj.Slug, Name: dj.Name, Verified: true}, nil
}
