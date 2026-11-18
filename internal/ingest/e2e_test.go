package ingest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/ingest"
	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/upstream"
	"github.com/splatboard/icegator/internal/upstreamtest"
)

// rig is Icegator wired to a fake station.
type rig struct {
	t       *testing.T
	station *upstreamtest.Station
	ctl     *controller.Controller
	addr    string
}

type rigOpt struct {
	forward bool
	policy  controller.Policy
	checker auth.Checker // overrides the real station check
}

var stationAccounts = map[string]string{"icegator": "shared-pw", "alice": "alice-pw", "bob": "bob-pw"}

func newRig(t *testing.T, o rigOpt) *rig {
	t.Helper()
	station, err := upstreamtest.Start("127.0.0.1:0", stationAccounts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(station.Close)
	up, err := upstream.New(station.URL())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opt := controller.Options{
		Upstream: up, Account: upstream.Credentials{Username: "icegator", Password: "shared-pw"},
		Policy: o.policy, Log: log, RetryDelay: 20 * time.Millisecond,
	}
	var a auth.Authenticator
	var ctl *controller.Controller
	if o.forward {
		known, _ := auth.OpenKnown("")
		var checker auth.Checker = up
		if o.checker != nil {
			checker = o.checker
		}
		f := auth.NewForward(checker, known)
		f.CurrentTitle = func() (string, bool) { return ctl.OnAirTitle() }
		f.Unverified = func(ip string) (int, int) { return ctl.Unverified(ip) }
		opt.OnAccepted, opt.OnRejected = f.Confirm, f.Reject
		a = f
	} else {
		a, err = auth.NewLocal([]auth.DJ{
			{Slug: "alice", Name: "Alice", Key: "AAAA-2222"},
			{Slug: "bob", Name: "Bob", Key: "BBBB-3333"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctl = controller.New(opt)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &ingest.Server{Auth: a, Gate: ctl, Log: log, ReadyAfter: 200 * time.Millisecond}
	go srv.Serve(ln)
	return &rig{t: t, station: station, ctl: ctl, addr: ln.Addr().String()}
}

// dj is a fake source client that streams the fixture in a loop, much
// faster than real time, until stopped.
type dj struct {
	conn   net.Conn
	status string
	stop   chan struct{}
	done   chan struct{}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// connect opens a source connection; requestLine is e.g. "PUT / HTTP/1.1".
func (r *rig) connect(requestLine, user, pass string, extra ...string) *dj {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "%s\r\nAuthorization: %s\r\nContent-Type: audio/mpeg\r\n", requestLine, basic(user, pass))
	for _, h := range extra {
		fmt.Fprintf(conn, "%s\r\n", h)
	}
	io.WriteString(conn, "\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		r.t.Fatalf("no response to %q: %v", requestLine, err)
	}
	d := &dj{conn: conn, status: strings.TrimSpace(status), stop: make(chan struct{}), done: make(chan struct{})}
	if !strings.Contains(d.status, " 200 ") && !strings.Contains(d.status, " 100 ") {
		close(d.done)
		return d
	}
	data, err := os.ReadFile("../../testdata/tone-2s.mp3")
	if err != nil {
		r.t.Fatal(err)
	}
	go func() {
		defer close(d.done)
		for {
			fr := mp3.NewReader(bytes.NewReader(data))
			for {
				f, err := fr.Next()
				if err != nil {
					break
				}
				select {
				case <-d.stop:
					return
				default:
				}
				if _, err := conn.Write(f.Data); err != nil {
					return
				}
				time.Sleep(time.Millisecond) // ~26x real time
			}
		}
	}()
	return d
}

func (r *rig) put(user, pass string) *dj { return r.connect("PUT / HTTP/1.1", user, pass) }

func (d *dj) hangUp() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	<-d.done
	d.conn.Close()
}

func (r *rig) title(user, pass, song string) string {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	defer conn.Close()
	q := url.Values{"mode": {"updinfo"}, "mount": {"/"}, "song": {song}}
	fmt.Fprintf(conn, "GET /admin/metadata?%s HTTP/1.0\r\nAuthorization: %s\r\n\r\n", q.Encode(), basic(user, pass))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(status)
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s; status: %+v", what, r.states())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// states maps username to state.
func (r *rig) states() map[string]controller.State {
	m := map[string]controller.State{}
	for _, d := range r.ctl.Status().DJs {
		m[d.Username] = d.State
	}
	return m
}

func (r *rig) waitState(user string, want controller.State) {
	r.t.Helper()
	r.waitFor(fmt.Sprintf("%s to be %s", user, want), func() bool { return r.states()[user] == want })
}

// waitLive waits for the station to have a live source that has received
// audio, and returns it.
func (r *rig) waitLive(n int) *upstreamtest.Source {
	r.t.Helper()
	var src *upstreamtest.Source
	r.waitFor(fmt.Sprintf("station source #%d with audio", n), func() bool {
		s := r.station.Sources()
		if len(s) != n {
			return false
		}
		src = s[n-1]
		return len(src.Bytes()) > 4000
	})
	return src
}

func (r *rig) waitTitle(want upstreamtest.Title) {
	r.t.Helper()
	r.waitFor(fmt.Sprintf("station title %+v (have %+v)", want, r.station.Titles()), func() bool {
		t := r.station.Titles()
		return len(t) > 0 && t[len(t)-1] == want
	})
}

// assertPassthrough checks that what the station received is whole frames
// and nothing else. Icegator must start on a frame boundary and must never
// insert a frame, alter one, or drop part of one.
func assertPassthrough(t *testing.T, got []byte) {
	t.Helper()
	data, _ := os.ReadFile("../../testdata/tone-2s.mp3")
	valid := map[string]bool{}
	fr := mp3.NewReader(bytes.NewReader(data))
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		valid[string(f.Data)] = true
	}
	fr = mp3.NewReader(bytes.NewReader(got))
	n, total := 0, 0
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		if !valid[string(f.Data)] {
			t.Fatalf("frame %d at the station is not one the DJ sent", n)
		}
		n++
		total += len(f.Data)
	}
	// The final frame may be cut short by the disconnect.
	if fr.Skipped() != 0 || len(got)-total > 1500 || n < 5 {
		t.Fatalf("station got %d bytes: %d whole frames (%d bytes), %d skipped", len(got), n, total, fr.Skipped())
	}
}

func TestQueueLocalAuth(t *testing.T) {
	r := newRig(t, rigOpt{})

	alice := r.put("alice", "aaaa2222") // keys forgive case and hyphens
	r.waitState("alice", controller.StateOnAir)
	src := r.waitLive(1)
	if src.Username != "icegator" {
		t.Fatalf("station saw %q, want the shared account", src.Username)
	}
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Alice"}) // fallback

	if s := r.title("alice", "AAAA-2222", "Artist - Song"); !strings.Contains(s, "200") {
		t.Fatalf("title update: %s", s)
	}
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Artist - Song"})

	bob := r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	// A held DJ's title is remembered, not sent.
	r.title("bob", "BBBB-3333", "Bob's Opener")
	time.Sleep(50 * time.Millisecond)
	if got := r.station.Titles(); got[len(got)-1].Song != "Artist - Song" {
		t.Fatalf("queued DJ's title reached the station: %+v", got)
	}
	if len(r.station.Sources()) != 1 {
		t.Fatal("queued DJ opened a station connection")
	}

	alice.hangUp()
	r.waitState("bob", controller.StateOnAir)
	r.waitLive(2)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Bob's Opener"})
	r.waitFor("first source to end", src.Ended)
	assertPassthrough(t, src.Bytes())

	bob.hangUp()
	r.waitFor("station to go idle", func() bool { return r.station.Live() == nil })
	if r.ctl.Status().OnAir != "" {
		t.Fatal("controller still thinks someone is on air")
	}
}

func TestStatusWithNobodyConnected(t *testing.T) {
	r := newRig(t, rigOpt{})
	b, err := json.Marshal(r.ctl.Status())
	if err != nil {
		t.Fatal(err)
	}
	// The dashboard iterates over this list, so it must never be null.
	if !strings.Contains(string(b), `"djs":[]`) {
		t.Fatalf("status with nobody connected: %s", b)
	}
}

func TestLegacySourceRequestAndExpect(t *testing.T) {
	r := newRig(t, rigOpt{})
	d := r.connect("SOURCE /live ICE/1.0", "alice", "AAAA-2222")
	if d.status != "HTTP/1.0 200 OK" {
		t.Fatalf("SOURCE got %q", d.status)
	}
	r.waitLive(1)
	d.hangUp()
	r.waitFor("idle", func() bool { return r.station.Live() == nil })

	d = r.connect("PUT /live HTTP/1.1", "alice", "AAAA-2222", "Expect: 100-continue")
	if d.status != "HTTP/1.1 100 Continue" {
		t.Fatalf("PUT with Expect got %q", d.status)
	}
	r.waitLive(2)
}

func TestPasswordOnlyClients(t *testing.T) {
	r := newRig(t, rigOpt{})
	// With the username stuck at "source", the DJ is found by key alone.
	d := r.put("source", "BBBB-3333")
	r.waitState("bob", controller.StateOnAir)
	d.hangUp()
	// Both parts packed into the password.
	r.put("source", "alice:AAAA-2222")
	r.waitState("alice", controller.StateOnAir)
}

func TestLocalAuthRejectsAndLocksOut(t *testing.T) {
	r := newRig(t, rigOpt{})
	for i := 0; i < 5; i++ {
		if d := r.put("alice", "WRONG-KEY"); !strings.Contains(d.status, "401") {
			t.Fatalf("attempt %d: %q", i, d.status)
		}
	}
	// Locked out now, even with the right key.
	if d := r.put("alice", "AAAA-2222"); !strings.Contains(d.status, "403") {
		t.Fatalf("after lockout: %q", d.status)
	}
	if d := r.connect("PUT / HTTP/1.1", "bob", "BBBB-3333", "Content-Type: audio/ogg"); !strings.Contains(d.status, "401") && !strings.Contains(d.status, "403") {
		// (the second Content-Type header is ignored; the address is locked)
		t.Fatalf("locked address: %q", d.status)
	}
}

func TestRejectsNonMP3(t *testing.T) {
	r := newRig(t, rigOpt{})
	conn, _ := net.Dial("tcp", r.addr)
	defer conn.Close()
	fmt.Fprintf(conn, "PUT / HTTP/1.1\r\nAuthorization: %s\r\nContent-Type: application/ogg\r\n\r\n", basic("alice", "AAAA-2222"))
	status, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(status, "415") {
		t.Fatalf("got %q", status)
	}
}

func TestManualHandoffAndOperatorActions(t *testing.T) {
	r := newRig(t, rigOpt{})
	if r.ctl.Handoff() {
		t.Fatal("handoff with nobody connected reported success")
	}
	r.put("alice", "AAAA-2222")
	r.waitState("alice", controller.StateOnAir)
	first := r.waitLive(1)
	if r.ctl.Handoff() {
		t.Fatal("handoff with nobody waiting reported success")
	}
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)

	if !r.ctl.Handoff() {
		t.Fatal("handoff refused")
	}
	r.waitState("bob", controller.StateOnAir)
	r.waitState("alice", controller.StateHeld)
	r.waitLive(2)
	if !first.Ended() {
		t.Fatal("old station connection still open after handoff")
	}

	// Pulling the on-air DJ leaves the air empty, because alice is held
	// rather than queued.
	st := r.ctl.Status()
	if err := r.ctl.Pull(st.OnAir); err != nil {
		t.Fatal(err)
	}
	r.waitFor("station idle", func() bool { return r.station.Live() == nil })
	if s := r.states(); s["alice"] != controller.StateHeld || s["bob"] != controller.StateHeld {
		t.Fatalf("states %+v", s)
	}

	// An operator can put a held DJ back.
	for _, d := range r.ctl.Status().DJs {
		if d.Username == "alice" {
			if err := r.ctl.Air(d.SessionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.waitState("alice", controller.StateOnAir)
	r.waitLive(3)
}

func TestAutoPolicyInAQueue(t *testing.T) {
	r := newRig(t, rigOpt{policy: controller.PolicyAuto})
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateOnAir)
	r.waitState("alice", controller.StateHeld)
	r.waitLive(2)
}

func TestStationDropReconnects(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.station.Kick()
	r.waitLive(2)
	r.waitState("alice", controller.StateOnAir)
}

func TestForwardedCredentials(t *testing.T) {
	r := newRig(t, rigOpt{forward: true})

	// A wrong password is refused at the door, not at air time.
	if d := r.put("alice", "not-her-password"); !strings.Contains(d.status, "401") {
		t.Fatalf("bad password: %q", d.status)
	}
	if len(r.station.Sources()) != 0 {
		t.Fatal("checking credentials opened a source connection")
	}

	alice := r.put("alice", "alice-pw")
	r.waitState("alice", controller.StateOnAir)
	src := r.waitLive(1)
	if src.Username != "alice" {
		t.Fatalf("station saw %q, want alice's own account", src.Username)
	}
	r.waitTitle(upstreamtest.Title{Username: "alice", Song: "alice"})
	r.title("alice", "alice-pw", "Song One")
	r.waitTitle(upstreamtest.Title{Username: "alice", Song: "Song One"})

	// Bob connects while alice is live. Checking him re-sends the title
	// that is already showing, so listeners see no change.
	bob := r.put("source", "bob:bob-pw")
	r.waitState("bob", controller.StateQueued)
	for _, title := range r.station.Titles() {
		if title.Song != "alice" && title.Song != "Song One" {
			t.Fatalf("credential check leaked a title: %+v", r.station.Titles())
		}
	}
	if st := r.ctl.Status(); !st.DJs[1].Verified {
		t.Fatal("bob should be verified")
	}

	// Another DJ's password does not let you set their title.
	if s := r.title("alice", "bob-pw", "Hijack"); strings.Contains(s, "200") {
		t.Fatalf("title with wrong password: %s", s)
	}

	alice.hangUp()
	src2 := r.waitLive(2)
	if src2.Username != "bob" {
		t.Fatalf("station saw %q, want bob", src2.Username)
	}
	bob.hangUp()
}

// unreachable is a credential check that can never reach the station.
type unreachable struct{}

func (unreachable) Check(context.Context, upstream.Credentials, string) (bool, error) {
	return false, errors.New("station unreachable")
}

func TestForwardUnverified(t *testing.T) {
	r := newRig(t, rigOpt{forward: true, checker: unreachable{}})

	// Unchecked DJs are admitted as unverified, up to the per-address cap.
	mallory := r.put("mallory", "guess")
	// Nobody better is waiting, so she is put on air; the station refuses
	// her and she is disconnected.
	<-mallory.done
	r.waitFor("mallory to be dropped", func() bool { return len(r.states()) == 0 })
	if len(r.station.Sources()) != 0 {
		t.Fatal("station accepted a bad login")
	}

	// Alice (unverified, but genuine) gets on; that verifies her.
	alice := r.put("alice", "alice-pw")
	r.waitLive(1)
	r.waitFor("alice verified", func() bool { return r.ctl.Status().DJs[0].Verified })

	// Two more unverified from this address are allowed, a third is not.
	r.put("x1", "pw")
	r.put("x2", "pw")
	r.waitState("x2", controller.StateQueued)
	if d := r.put("x3", "pw"); !strings.Contains(d.status, "403") {
		t.Fatalf("over the cap: %q", d.status)
	}

	// When alice leaves, the unverified DJs each get their turn and are
	// each refused by the station.
	alice.hangUp()
	r.waitFor("x1 and x2 to be dropped", func() bool { return len(r.states()) == 0 })
	if n := len(r.station.Sources()); n != 1 {
		t.Fatalf("station accepted %d sources, want 1", n)
	}

	// Alice is remembered as good despite the outage, so she is admitted
	// as verified even with the cap reached.
	r.put("y1", "pw")
	r.put("y2", "pw")
	r.put("alice", "alice-pw")
	r.waitFor("alice back on air", func() bool { return r.states()["alice"] == controller.StateOnAir })
	for _, d := range r.ctl.Status().DJs {
		if d.Username == "alice" && !d.Verified {
			t.Fatal("alice should be remembered as verified")
		}
	}
}
