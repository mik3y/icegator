package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/session"
)

type fakeGate struct {
	calls   []string
	waiting bool
}

func (g *fakeGate) Status() controller.Status {
	return controller.Status{OnAir: "s1", DJs: []controller.DJ{{SessionID: "s1", Username: "alice", State: controller.StateOnAir}}}
}
func (g *fakeGate) Levels() map[string]*session.Level {
	return map[string]*session.Level{"s1": {Loudness: -14, Verdict: "good"}}
}
func (g *fakeGate) Handoff() bool { g.calls = append(g.calls, "handoff"); return g.waiting }
func (g *fakeGate) Air(id, showID string) error {
	g.calls = append(g.calls, strings.TrimSpace("air "+id+" "+showID))
	if id == "nope" {
		return errors.New("no such session")
	}
	return nil
}
func (g *fakeGate) Pull(id string) error { g.calls = append(g.calls, "pull "+id); return nil }
func (g *fakeGate) Kick(id string) error { g.calls = append(g.calls, "kick "+id); return nil }

func setup(t *testing.T) (*httptest.Server, *http.Client, *fakeGate, *Server) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter22"), bcrypt.MinCost)
	gate := &fakeGate{}
	s := &Server{
		Info: Info{Station: "Test FM", Mode: "standalone"}, Gate: gate,
		Operators:     map[string]string{"mike": string(hash)},
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		LevelInterval: 5 * time.Millisecond, StatusInterval: time.Hour,
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}, gate, s
}

func post(t *testing.T, c *http.Client, url, body string, csrf bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if csrf {
		req.Header.Set("X-Icegator", "1")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func signIn(t *testing.T, c *http.Client, base string) {
	t.Helper()
	if code, body := post(t, c, base+"/api/login", `{"username":"mike","password":"hunter22"}`, true); code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
}

func TestLoginRequired(t *testing.T) {
	ts, c, gate, _ := setup(t)
	if code, _ := get(t, c, ts.URL+"/api/status"); code != 401 {
		t.Fatalf("status without login: %d", code)
	}
	if code, _ := post(t, c, ts.URL+"/api/handoff", "", true); code != 401 {
		t.Fatalf("handoff without login: %d", code)
	}
	if code, _ := post(t, c, ts.URL+"/api/login", `{"username":"mike","password":"wrong"}`, true); code != 401 {
		t.Fatalf("bad password: %d", code)
	}
	if code, _ := post(t, c, ts.URL+"/api/login", `{"username":"nobody","password":"hunter22"}`, true); code != 401 {
		t.Fatalf("unknown user: %d", code)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("gate was called: %v", gate.calls)
	}

	signIn(t, c, ts.URL)
	code, body := get(t, c, ts.URL+"/api/status")
	if code != 200 || !strings.Contains(body, `"station":"Test FM"`) || !strings.Contains(body, `"on_air":"s1"`) {
		t.Fatalf("status: %d %s", code, body)
	}

	post(t, c, ts.URL+"/api/logout", "", true)
	if code, _ := get(t, c, ts.URL+"/api/status"); code != 401 {
		t.Fatalf("status after logout: %d", code)
	}
}

func TestActions(t *testing.T) {
	ts, c, gate, _ := setup(t)
	signIn(t, c, ts.URL)

	// A cross-site form post would carry the cookie but not the header.
	if code, _ := post(t, c, ts.URL+"/api/handoff", "", false); code != 403 {
		t.Fatalf("post without csrf header: %d", code)
	}
	if code, _ := post(t, c, ts.URL+"/api/handoff", "", true); code != 409 {
		t.Fatalf("handoff with nobody waiting: %d", code)
	}
	gate.waiting = true
	if code, _ := post(t, c, ts.URL+"/api/handoff", "", true); code != 200 {
		t.Fatalf("handoff: %d", code)
	}
	for _, a := range []string{"air", "pull", "kick"} {
		if code, _ := post(t, c, ts.URL+"/api/sessions/s1/"+a, "", true); code != 200 {
			t.Fatalf("%s: %d", a, code)
		}
	}
	if code, _ := post(t, c, ts.URL+"/api/sessions/s1/air", `{"show_id":"show9"}`, true); code != 200 {
		t.Fatalf("air with a show: %d", code)
	}
	if code, body := post(t, c, ts.URL+"/api/sessions/nope/air", "", true); code != 409 || !strings.Contains(body, "no such session") {
		t.Fatalf("air unknown: %d %s", code, body)
	}
	if code, _ := post(t, c, ts.URL+"/api/sessions/s1/explode", "", true); code != 404 {
		t.Fatalf("unknown action: %d", code)
	}
	want := "handoff,handoff,air s1,pull s1,kick s1,air s1 show9,air nope"
	if got := strings.Join(gate.calls, ","); got != want {
		t.Fatalf("gate calls %q, want %q", got, want)
	}
}

func TestLoginLockout(t *testing.T) {
	ts, c, _, _ := setup(t)
	for i := 0; i < 8; i++ {
		post(t, c, ts.URL+"/api/login", `{"username":"mike","password":"wrong"}`, true)
	}
	if code, _ := post(t, c, ts.URL+"/api/login", `{"username":"mike","password":"hunter22"}`, true); code != 429 {
		t.Fatalf("after 8 failures: %d", code)
	}
}

func TestEvents(t *testing.T) {
	ts, c, _, s := setup(t)
	signIn(t, c, ts.URL)
	resp, err := c.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	events := make(chan [2]string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{name, v}
			}
		}
	}()
	next := func(want string) string {
		t.Helper()
		timeout := time.After(2 * time.Second)
		for {
			select {
			case e := <-events:
				if e[0] == want {
					return e[1]
				}
			case <-timeout:
				t.Fatalf("no %q event", want)
			}
		}
	}

	var st statusPayload
	if err := json.Unmarshal([]byte(next("status")), &st); err != nil || st.OnAir != "s1" || st.Info.Station != "Test FM" {
		t.Fatalf("first status: %+v %v", st, err)
	}
	var lv map[string]session.Level
	if err := json.Unmarshal([]byte(next("levels")), &lv); err != nil || lv["s1"].Verdict != "good" {
		t.Fatalf("levels: %+v %v", lv, err)
	}
	// With the periodic refresh an hour away, a second status event can
	// only come from Changed.
	s.Changed()
	next("status")
}
