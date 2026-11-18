package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/session"
)

// destination is somewhere recordings are delivered to, which can be told
// to fail.
type destination struct {
	mu      sync.Mutex
	got     []Info
	audio   map[string][]byte // by file name
	failing error
}

func (d *destination) deliver(_ context.Context, info Info, audioPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failing != nil {
		return d.failing
	}
	b, err := os.ReadFile(audioPath)
	if err != nil {
		return err
	}
	if d.audio == nil {
		d.audio = map[string][]byte{}
	}
	d.audio[info.File] = b
	d.got = append(d.got, info)
	return nil
}

func (d *destination) infos() []Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Info(nil), d.got...)
}

func (d *destination) fail(err error) {
	d.mu.Lock()
	d.failing = err
	d.mu.Unlock()
}

func frames(t *testing.T) [][]byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/tone-2s.mp3")
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	fr := mp3.NewReader(bytes.NewReader(data))
	for {
		f, err := fr.Next()
		if err != nil {
			return out
		}
		out = append(out, append([]byte(nil), f.Data...))
	}
}

func setup(t *testing.T, opt Options) (*Recorder, *destination) {
	t.Helper()
	dest := &destination{}
	if opt.Dir == "" {
		opt.Dir = t.TempDir()
	}
	opt.Deliver = dest.deliver
	opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if opt.MinDuration == 0 {
		opt.MinDuration = time.Second
	}
	if opt.RetryDelay == 0 {
		opt.RetryDelay = 20 * time.Millisecond
	}
	r, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return r, dest
}

func run(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spool lists what is in the spool directory itself, by name.
func spool(r *Recorder) []string {
	entries, _ := os.ReadDir(r.opt.Dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

var alice = Subject{DJ: session.Identity{Username: "alice", Name: "Alice"}, SessionID: "abc12345", TurnID: "abc12345-4", ShowID: "show_1", Show: "Morning Glory"}

func record(t *testing.T, r *Recorder) *Recording {
	t.Helper()
	rec, err := r.Start(alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames(t) {
		rec.Write(f)
	}
	rec.Close()
	return rec
}

func TestRecordAndDeliver(t *testing.T) {
	r, dest := setup(t, Options{})
	run(t, r)

	rec, err := r.Start(alice)
	if err != nil {
		t.Fatal(err)
	}
	fs := frames(t)
	var want []byte
	rec.Title("Alice")
	for i, f := range fs {
		if i == 20 {
			rec.Title("Track One")
		}
		if i == 40 {
			rec.Gap()
			rec.Title("Track One") // re-sent after a reconnect; not a new mark
		}
		rec.Write(f)
		want = append(want, f...)
	}
	rec.Close()
	rec.Write(fs[0]) // ignored after Close

	waitFor(t, "delivery", func() bool { return len(dest.infos()) == 1 })
	info := dest.infos()[0]
	if !strings.HasSuffix(info.File, "-alice-abc12345-4.mp3") {
		t.Fatalf("file %q", info.File)
	}
	if !bytes.Equal(dest.audio[info.File], want) {
		t.Fatal("delivered audio differs from the frames written")
	}
	if info.DJ != "alice" || info.DJName != "Alice" || info.TurnID != "abc12345-4" || info.ShowID != "show_1" ||
		info.Show != "Morning Glory" || info.Bytes != int64(len(want)) || info.Bitrate != 128000 {
		t.Fatalf("info %+v", info)
	}
	if info.Seconds < 1.9 || info.Seconds > 2.1 {
		t.Fatalf("seconds %v", info.Seconds)
	}
	if len(info.Titles) != 2 || info.Titles[0].OffsetSeconds != 0 || info.Titles[1].Title != "Track One" ||
		info.Titles[1].OffsetSeconds < 0.5 || info.Titles[1].OffsetSeconds > 0.55 {
		t.Fatalf("titles %+v", info.Titles)
	}
	if len(info.Gaps) != 1 || info.Gaps[0].OffsetSeconds < 1.0 || info.Gaps[0].OffsetSeconds > 1.1 {
		t.Fatalf("gaps %+v", info.Gaps)
	}
	// Once it has arrived, nothing of it is left here.
	waitFor(t, "the local copy to be deleted", func() bool { return len(spool(r)) == 0 })
}

func TestShortRecordingDiscarded(t *testing.T) {
	r, dest := setup(t, Options{MinDuration: 10 * time.Second})
	run(t, r)
	record(t, r)
	time.Sleep(60 * time.Millisecond)
	if left := spool(r); len(left) != 0 || len(dest.infos()) != 0 {
		t.Fatalf("spool %v, delivered %d", left, len(dest.infos()))
	}
}

func TestFailedDeliveryIsRetriedAndTheFileKept(t *testing.T) {
	r, dest := setup(t, Options{})
	dest.fail(errors.New("destination unreachable"))
	run(t, r)
	record(t, r)
	time.Sleep(80 * time.Millisecond)
	if left := spool(r); len(left) != 2 {
		t.Fatalf("spool should still hold audio and info, has %v", left)
	}
	dest.fail(nil)
	waitFor(t, "a later attempt to succeed", func() bool { return len(dest.infos()) == 1 })
	waitFor(t, "the local copy to be deleted", func() bool { return len(spool(r)) == 0 })
}

func TestUndeliverableIsSetAsideNotDeletedAndNotRetried(t *testing.T) {
	r, dest := setup(t, Options{})
	dest.fail(fmt.Errorf("%w: too large", ErrUndeliverable))
	run(t, r)
	record(t, r)
	aside := filepath.Join(r.opt.Dir, "undelivered")
	waitFor(t, "the recording to be set aside", func() bool {
		kept, _ := filepath.Glob(filepath.Join(aside, "*"))
		return len(kept) == 2 && len(spool(r)) == 0
	})
	// It would be accepted now, but it is not offered again.
	dest.fail(nil)
	time.Sleep(80 * time.Millisecond)
	if len(dest.infos()) != 0 {
		t.Fatal("a recording that was set aside was delivered again")
	}
}

func TestRecoversAfterCrash(t *testing.T) {
	// A crash leaves audio in the spool with no info file beside it.
	dir := t.TempDir()
	var audio []byte
	for _, f := range frames(t) {
		audio = append(audio, f...)
	}
	audio = append(audio, 0xFF, 0xFB, 0x90) // and a frame cut off mid-write
	name := "20261118-210000-night-owl-0a1b2c3d-7.mp3"
	os.WriteFile(filepath.Join(dir, name), audio, 0o600)

	r, dest := setup(t, Options{Dir: dir})
	// A recording that starts straight after startup must not be mistaken
	// for another casualty of the crash.
	rec, _ := r.Start(alice)
	run(t, r)
	waitFor(t, "the recovered recording to be delivered", func() bool { return len(dest.infos()) == 1 })
	info := dest.infos()[0]
	if !info.Recovered || info.File != name || info.DJ != "night-owl" || info.SessionID != "0a1b2c3d" ||
		info.TurnID != "0a1b2c3d-7" || info.Seconds < 1.9 {
		t.Fatalf("info %+v", info)
	}
	if !info.StartedAt.Equal(time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("started %v", info.StartedAt)
	}
	for _, f := range frames(t) {
		rec.Write(f)
	}
	rec.Close()
	waitFor(t, "the new recording to be delivered as well", func() bool { return len(dest.infos()) == 2 })
}

func TestInfoOnDiskIsReadable(t *testing.T) {
	// The info file is what a person rescuing a recording by hand has to
	// go on, so it should say what the recording is.
	r, dest := setup(t, Options{})
	dest.fail(errors.New("not yet"))
	record(t, r)
	infos, _ := filepath.Glob(filepath.Join(r.opt.Dir, "*.json"))
	if len(infos) != 1 {
		t.Fatalf("info files %v", infos)
	}
	b, _ := os.ReadFile(infos[0])
	var info Info
	if err := json.Unmarshal(b, &info); err != nil || info.Show != "Morning Glory" || info.DJ != "alice" {
		t.Fatalf("%s: %v", b, err)
	}
}
