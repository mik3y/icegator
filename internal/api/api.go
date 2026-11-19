// Package api serves the operator dashboard. That is a JSON API, a live
// event stream, and the embedded web app.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/session"
)

// Gate is what the dashboard can see and do.
type Gate interface {
	Status() controller.Status
	Levels() map[string]*session.Level
	Handoff() bool
	Air(sessionID, showID string) error
	Pull(sessionID string) error
	Kick(sessionID string) error
}

// Info is static description shown in the dashboard.
type Info struct {
	Station string `json:"station"`
	// Provider is where the DJs come from, "static" or "streamlover".
	Provider string `json:"provider"`
	// Scheduled reports whether the provider supplies a schedule.
	Scheduled bool   `json:"scheduled"`
	Policy    string `json:"policy"` // "auto" or "manual"
	// Overrun is "none", "strict" or a duration. It applies when there is
	// a schedule.
	Overrun string `json:"overrun,omitempty"`
	// Version is the release the binary was built from.
	Version string `json:"version,omitempty"`
}

// Server is the dashboard's HTTP handler.
type Server struct {
	Info Info
	Gate Gate
	// Operators maps username to bcrypt password hash.
	Operators map[string]string
	// Web is the built dashboard app; nil serves the API only.
	Web fs.FS
	Log *slog.Logger

	// LevelInterval and StatusInterval pace the event stream. Defaults
	// 250ms, which is how often the meter has a new reading, and 1s.
	LevelInterval  time.Duration
	StatusInterval time.Duration

	once     sync.Once
	mux      *http.ServeMux
	limit    *auth.Limiter
	mu       sync.Mutex
	sessions map[string]login
	dummy    []byte // a real hash to compare against for unknown usernames
	gen      atomic.Uint64
}

type login struct {
	username string
	expires  time.Time
}

const (
	cookieName = "icegator_session"
	loginTTL   = 30 * 24 * time.Hour
	// csrfHeader must accompany every state-changing request. Browsers
	// will not let another site send a custom header without asking us
	// first (CORS preflight), which we never approve.
	csrfHeader = "X-Icegator"
)

// Changed tells connected dashboards that gate state has changed. It is
// cheap and never blocks; wire it to controller.Options.OnChange.
func (s *Server) Changed() { s.gen.Add(1) }

func (s *Server) init() {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.LevelInterval == 0 {
		s.LevelInterval = 250 * time.Millisecond
	}
	if s.StatusInterval == 0 {
		s.StatusInterval = time.Second
	}
	s.limit = auth.NewLimiter(8, 5*time.Minute)
	s.sessions = map[string]login{}
	pw := make([]byte, 16)
	rand.Read(pw)
	s.dummy, _ = bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)

	m := http.NewServeMux()
	m.HandleFunc("POST /api/login", s.login)
	m.HandleFunc("POST /api/logout", s.logout)
	m.HandleFunc("GET /api/me", s.authed(s.me))
	m.HandleFunc("GET /api/status", s.authed(s.status))
	m.HandleFunc("GET /api/events", s.authed(s.events))
	m.HandleFunc("POST /api/handoff", s.authed(s.handoff))
	m.HandleFunc("POST /api/sessions/{id}/{action}", s.authed(s.sessionAction))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusNotFound, "no such endpoint") })
	if s.Web != nil {
		m.Handle("/", spa(s.Web))
	}
	s.mux = m
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.once.Do(s.init)
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
		fail(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string { return auth.RemoteIP(r.RemoteAddr) }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "expected a JSON body with username and password")
		return
	}
	ip := "ip:" + clientIP(r)
	if s.limit.Blocked(ip) {
		fail(w, http.StatusTooManyRequests, "too many attempts; try again in a few minutes")
		return
	}
	hash, ok := []byte(s.Operators[req.Username]), true
	if len(hash) == 0 {
		// Spend the same time as a real check so usernames can't be probed.
		hash, ok = s.dummy, false
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) != nil || !ok {
		s.limit.Fail(ip)
		s.Log.Warn("dashboard login failed", "username", req.Username, "remote", clientIP(r))
		fail(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	now := time.Now()
	for t, l := range s.sessions {
		if now.After(l.expires) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = login{username: req.Username, expires: now.Add(loginTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", MaxAge: int(loginTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	s.Log.Info("dashboard login", "username", req.Username, "remote", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"username": req.Username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) operator(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.sessions[c.Value]
	if !ok || time.Now().After(l.expires) {
		return "", false
	}
	return l.username, true
}

type authedHandler func(w http.ResponseWriter, r *http.Request, operator string)

func (s *Server) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		op, ok := s.operator(r)
		if !ok {
			fail(w, http.StatusUnauthorized, "sign in first")
			return
		}
		h(w, r, op)
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, op string) {
	writeJSON(w, http.StatusOK, map[string]any{"username": op, "info": s.Info})
}

type statusPayload struct {
	Info Info `json:"info"`
	controller.Status
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ string) {
	writeJSON(w, http.StatusOK, statusPayload{s.Info, s.Gate.Status()})
}

func (s *Server) handoff(w http.ResponseWriter, r *http.Request, op string) {
	if !s.Gate.Handoff() {
		fail(w, http.StatusConflict, "nobody is waiting to take over")
		return
	}
	s.Log.Info("operator handoff", "operator", op)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sessionAction(w http.ResponseWriter, r *http.Request, op string) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var err error
	switch action {
	case "air":
		// The body is optional. It may name a show for the DJ to
		// broadcast as when they are being put on air off-schedule.
		var req struct {
			ShowID string `json:"show_id"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		err = s.Gate.Air(id, req.ShowID)
	case "pull":
		err = s.Gate.Pull(id)
	case "kick":
		err = s.Gate.Kick(id)
	default:
		fail(w, http.StatusNotFound, "no such action")
		return
	}
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	s.Log.Info("operator action", "operator", op, "action", action, "session", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// events is a server-sent event stream. A "status" event goes out whenever
// gate state changes, and at least every StatusInterval so that stream
// statistics stay fresh. A "levels" event goes out every LevelInterval
// while any DJ is connected.
func (s *Server) events(w http.ResponseWriter, r *http.Request, _ string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}

	tick := time.NewTicker(s.LevelInterval)
	defer tick.Stop()
	var lastGen uint64
	var lastStatus time.Time
	first := true
	for {
		if gen := s.gen.Load(); first || gen != lastGen || time.Since(lastStatus) >= s.StatusInterval {
			send("status", statusPayload{s.Info, s.Gate.Status()})
			lastGen, lastStatus, first = gen, time.Now(), false
		}
		if lv := s.Gate.Levels(); len(lv) > 0 {
			send("levels", lv)
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// spa serves the built web app. A path that is not a file in it is treated
// as a client-side route and gets index.html, unless it looks like a file
// request, which gets a 404 so that a missing asset fails loudly.
func spa(web fs.FS) http.Handler {
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		switch _, err := fs.Stat(web, p); {
		case err != nil && (strings.HasPrefix(p, "assets/") || path.Ext(p) != ""):
			http.NotFound(w, r)
			return
		case err != nil:
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		case strings.HasPrefix(p, "assets/"):
			// Built asset names contain a content hash.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
