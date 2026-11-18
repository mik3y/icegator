package ingest_test

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	gomp3 "github.com/hajimehoshi/go-mp3"

	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/upstreamtest"
)

// keepRig is a rig that hands the station connection from DJ to DJ, and
// keeps a tape of each turn.
func keepRig(t *testing.T) (*rig, func() []*tape) {
	var mu sync.Mutex
	var tapes []*tape
	r := newRig(t, rigOpt{keep: true, record: func(turn controller.Turn) (controller.Recording, error) {
		mu.Lock()
		defer mu.Unlock()
		tp := &tape{dj: turn.DJ.Username}
		tapes = append(tapes, tp)
		return tp, nil
	}})
	return r, func() []*tape {
		mu.Lock()
		defer mu.Unlock()
		return append([]*tape(nil), tapes...)
	}
}

func TestKeptConnectionSurvivesAHandoff(t *testing.T) {
	r, tapes := keepRig(t)
	r.put("alice", "AAAA-2222")
	src := r.waitLive(1)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Alice"})
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)

	before := len(src.Bytes())
	if !r.ctl.Handoff() {
		t.Fatal("handoff refused")
	}
	r.waitState("bob", controller.StateOnAir)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Bob"})
	r.waitFor("bob's audio to reach the station", func() bool { return len(src.Bytes()) > before+8000 })

	// The station never saw a disconnect.
	if n := len(r.station.Sources()); n != 1 || src.Ended() {
		t.Fatalf("station saw %d connections (first ended: %v), want one, still open", n, src.Ended())
	}
	// What it received is whole frames throughout, across the join.
	assertPassthrough(t, src.Bytes())

	// Bob's part starts with frames that need nothing from what came
	// before them, which is what keeps the join from clicking.
	r.waitFor("bob's recording", func() bool { return len(tapes()) == 2 })
	var bob []byte
	r.waitFor("bob's first recorded frames", func() bool {
		bob, _, _, _ = tapes()[1].snapshot()
		return len(bob) > 2000
	})
	first, err := mp3.NewReader(bytes.NewReader(bob)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := mp3.MainDataBegin(first.Data); !ok || n != 0 {
		t.Fatalf("bob's first frame on air borrows %d bytes from earlier frames", n)
	}
	// Decoded as the station would decode it, alice's audio followed by
	// bob's has no frame the decoder had to give up on.
	alicePart, _, _, _ := tapes()[0].snapshot()
	joined := append(append([]byte{}, alicePart...), bob...)
	if got, want := decodedFrames(t, joined), wholeFrames(joined); got != want {
		t.Fatalf("decoder produced %d frames of audio from %d frames of stream", got, want)
	}
	// And his recording is exactly what followed alice's on the wire.
	alice, _, _, closed := tapes()[0].snapshot()
	if !closed {
		t.Fatal("alice's recording is still open")
	}
	got := src.Bytes()
	if !bytes.HasPrefix(got, alice) || !bytes.HasPrefix(got[len(alice):], bob[:2000]) {
		t.Fatal("the station did not receive alice's audio followed directly by bob's")
	}
}

func TestKeptConnectionPassesToTheNextInTheQueue(t *testing.T) {
	r, _ := keepRig(t)
	alice := r.put("alice", "AAAA-2222")
	src := r.waitLive(1)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	alice.hangUp()
	r.waitState("bob", controller.StateOnAir)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Bob"})
	if n := len(r.station.Sources()); n != 1 || src.Ended() {
		t.Fatalf("station saw %d connections, want the one kept open", n)
	}
}

func TestKeptConnectionIsReleasedWhenNobodyFollows(t *testing.T) {
	r, _ := keepRig(t)
	alice := r.put("alice", "AAAA-2222")
	src := r.waitLive(1)
	alice.hangUp()
	// It is held briefly in case someone is about to take it, then let go
	// so that the station knows to carry on by itself.
	time.Sleep(200 * time.Millisecond)
	if src.Ended() {
		t.Fatal("the connection was dropped at once, leaving no time for a successor")
	}
	r.waitFor("the station connection to be released", src.Ended)

	// A DJ arriving later starts a connection of their own.
	r.put("bob", "BBBB-3333")
	r.waitLive(2)
}

func TestDifferentFormatGetsItsOwnConnection(t *testing.T) {
	r, _ := keepRig(t)
	alice := r.put("alice", "AAAA-2222") // 44.1 kHz stereo
	first := r.waitLive(1)
	r.connectFile("../../testdata/tone-22k-mono.mp3", "PUT / HTTP/1.1", "bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	alice.hangUp()

	second := r.waitLive(2)
	r.waitFor("the first connection to close", first.Ended)
	// Neither connection carries a mix of the two formats.
	for i, src := range []*upstreamtest.Source{first, second} {
		rates := map[int]bool{}
		fr := mp3.NewReader(bytes.NewReader(src.Bytes()))
		for {
			f, err := fr.Next()
			if err != nil {
				break
			}
			rates[f.Header.SampleRate] = true
		}
		if len(rates) != 1 {
			t.Fatalf("connection %d carried sample rates %v", i+1, rates)
		}
	}
}

func TestForwardedLoginsAlwaysReconnect(t *testing.T) {
	// Each DJ has to appear to the station as themselves, so there is no
	// connection that could be handed on.
	r := newRig(t, rigOpt{forward: true, keep: true})
	alice := r.put("alice", "alice-pw")
	r.waitLive(1)
	r.put("bob", "bob-pw")
	r.waitState("bob", controller.StateQueued)
	alice.hangUp()
	if src := r.waitLive(2); src.Username != "bob" {
		t.Fatalf("second connection is %q, want bob", src.Username)
	}
}

// wholeFrames counts the complete frames in a stream.
func wholeFrames(stream []byte) int {
	n := 0
	fr := mp3.NewReader(bytes.NewReader(stream))
	for {
		if _, err := fr.Next(); err != nil {
			return n
		}
		n++
	}
}

// decodedFrames counts the frames of audio a decoder gets out of a stream.
func decodedFrames(t *testing.T, stream []byte) int {
	t.Helper()
	dec, err := gomp3.NewDecoder(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	return len(pcm) / (1152 * 4)
}
