package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/upstream"
)

// Checker asks the station whether it accepts some credentials without
// going live. *upstream.Client implements it.
type Checker interface {
	Check(ctx context.Context, creds upstream.Credentials, title string) (bool, error)
}

// Forward admits anyone and passes their credentials on to the station.
//
// To avoid finding out about a wrong password at the moment a DJ goes on
// air, it asks the station straight away when it can. When it can't (the
// station is unreachable), it falls back on remembering which credentials
// the station has accepted before; anything else is admitted "unverified",
// and unverified connections are capped.
type Forward struct {
	Checker Checker
	// CurrentTitle returns the title now showing on the station when a DJ
	// is on air through Icegator. A check rewrites the title, so it must
	// re-send this one.
	CurrentTitle func() (title string, onAir bool)
	// Unverified counts unverified sessions already connected, in total
	// and from one address.
	Unverified func(ip string) (total, fromIP int)

	MaxUnverified      int // default 4
	MaxUnverifiedPerIP int // default 2

	known *Known
	limit *Limiter
}

// NewForward creates a forwarding authenticator that remembers accepted
// credentials in known.
func NewForward(checker Checker, known *Known) *Forward {
	return &Forward{
		Checker: checker, known: known, limit: NewLimiter(10, 5*time.Minute),
		MaxUnverified: 4, MaxUnverifiedPerIP: 2,
	}
}

func (f *Forward) Authenticate(ctx context.Context, username, password, ip string) (session.Identity, error) {
	username, password = SplitShoutcast(username, password)
	if username == "" || password == "" {
		return session.Identity{}, fmt.Errorf("%w: the username or the password was empty", ErrDenied)
	}
	if f.limit.Blocked("ip:" + ip) {
		return session.Identity{}, ErrLimited
	}
	creds := upstream.Credentials{Username: username, Password: password}
	id := session.Identity{Username: username, Upstream: &creds}

	title := username
	if f.CurrentTitle != nil {
		if t, onAir := f.CurrentTitle(); onAir {
			title = t
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok, err := f.Checker.Check(ctx, creds, title)
	switch {
	case err == nil && ok:
		f.known.Add(creds)
		id.Verified = true
		return id, nil
	case err == nil:
		f.known.Remove(creds)
		f.limit.Fail("ip:" + ip)
		return session.Identity{}, fmt.Errorf("%w: the station does not accept this username and password", ErrDenied)
	}

	// The station could not be asked.
	if f.known.Has(creds) {
		id.Verified = true
		return id, nil
	}
	if f.Unverified != nil {
		total, fromIP := f.Unverified(ip)
		if total >= f.MaxUnverified || fromIP >= f.MaxUnverifiedPerIP {
			return session.Identity{}, ErrLimited
		}
	}
	return id, nil
}

// Confirm records that the station accepted creds for a live connection.
func (f *Forward) Confirm(creds upstream.Credentials) { f.known.Add(creds) }

// Reject records that the station refused creds.
func (f *Forward) Reject(creds upstream.Credentials) { f.known.Remove(creds) }

// Known is the set of credentials the station has accepted, kept as salted
// hashes so the file does not reveal passwords.
type Known struct {
	mu     sync.Mutex
	path   string          // "" keeps the set in memory only
	Salt   string          `json:"salt"`
	Hashes map[string]bool `json:"hashes"`
}

// OpenKnown loads the set from path, creating it on first write. An empty
// path gives an in-memory set.
func OpenKnown(path string) (*Known, error) {
	k := &Known{path: path, Hashes: map[string]bool{}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(b, k); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if k.Salt == "" {
		salt := make([]byte, 16)
		rand.Read(salt)
		k.Salt = hex.EncodeToString(salt)
	}
	if k.Hashes == nil {
		k.Hashes = map[string]bool{}
	}
	return k, nil
}

func (k *Known) hash(c upstream.Credentials) string {
	sum := sha256.Sum256([]byte(k.Salt + "\x00" + c.Username + "\x00" + c.Password))
	return hex.EncodeToString(sum[:])
}

func (k *Known) Has(c upstream.Credentials) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.Hashes[k.hash(c)]
}

func (k *Known) Add(c upstream.Credentials) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if h := k.hash(c); !k.Hashes[h] {
		k.Hashes[h] = true
		k.save()
	}
}

func (k *Known) Remove(c upstream.Credentials) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if h := k.hash(c); k.Hashes[h] {
		delete(k.Hashes, h)
		k.save()
	}
}

func (k *Known) save() {
	if k.path == "" {
		return
	}
	b, _ := json.Marshal(k)
	tmp := k.path + ".tmp"
	os.MkdirAll(filepath.Dir(k.path), 0o700)
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, k.path)
	}
}
