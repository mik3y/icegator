package streamlover

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/recorder"
	"github.com/splatboard/icegator/internal/schedule"
	"github.com/splatboard/icegator/internal/session"
)

const secret = "slk_secret_test"

func digest(streamKey string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(streamKey))
	return hex.EncodeToString(mac.Sum(nil))
}

// fake stands in for StreamLover. Its replies have the shape of the real
// ones, as captured from a running instance.
type fake struct {
	*httptest.Server
	mu sync.Mutex
	// What it serves.
	hosts, shows, occurrences string
	// What it was sent, as "METHOD path body".
	calls []string
	// broadcasts it holds, by id.
	broadcasts map[string]map[string]any
	// taken, when set, is the broadcast every new one collides with.
	taken string
	// failing makes every request fail as an outage would.
	failing bool
	// uploads holds what was sent to each upload address, by audio file id,
	// and uploadAuth what Authorization header came with it.
	uploads    map[string][]byte
	uploadAuth map[string]string
	completed  map[string]map[string]any
	// uploadBase, when set, is put in front of the upload address handed
	// out, as when storage is on another host.
	uploadBase string
	// failAttach makes attaching a recording fail as an outage would.
	failAttach bool
}

func newFake(t *testing.T) *fake {
	f := &fake{
		broadcasts: map[string]map[string]any{}, uploads: map[string][]byte{},
		uploadAuth: map[string]string{}, completed: map[string]map[string]any{},
	}
	start := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	f.hosts = `[
	 {"id":"host_1","slug":"alice","name":"Alice","stream_key_hmac":"` + digest("AAAA2222") + `"},
	 {"id":"host_2","slug":"bob","name":"Bob","stream_key_hmac":"` + digest("BBBB3333") + `"},
	 {"id":"host_3","slug":"no-key","name":"No Key","stream_key_hmac":""}]`
	f.shows = `[
	 {"id":"show_mg","name":"Morning Glory","hosts":[{"id":"host_1","slug":"alice","name":"Alice"}]},
	 {"id":"show_duo","name":"The Duo","hosts":[{"id":"host_2","slug":"bob","name":"Bob"},{"id":"host_1","slug":"alice","name":"Alice"}]},
	 {"id":"show_auto","name":"Automation","hosts":[]}]`
	occ := func(sched, show, name string, from, mins int) string {
		s := start.Add(time.Duration(from) * time.Minute)
		return `{"schedule_id":"` + sched + `","rrule":"","show":{"id":"` + show + `","name":"` + name + `"},"start":"` +
			s.Format(time.RFC3339) + `","end":"` + s.Add(time.Duration(mins)*time.Minute).Format(time.RFC3339) +
			`","exception_id":null,"original_start":"` + s.Format(time.RFC3339) + `"}`
	}
	f.occurrences = "[" + occ("sched_1", "show_mg", "Morning Glory", 0, 60) + "," +
		occ("sched_2", "show_duo", "The Duo", 60, 60) + "," + occ("sched_3", "show_auto", "Automation", 120, 30) + "]"
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	// The address a file is uploaded to. Storage asks for no API key.
	if id, ok := strings.CutPrefix(r.URL.Path, "/storage/"); ok && r.Method == "PUT" {
		f.uploads[id], f.uploadAuth[id] = body, r.Header.Get("Authorization")
		if r.Header.Get("Content-Type") != "audio/mpeg" {
			http.Error(w, "wrong content type", http.StatusForbidden)
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+secret {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"detail":"Invalid API key."}`)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/station/ktest")
	f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+path+" "+string(body)))
	var in map[string]any
	json.Unmarshal(body, &in)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "broadcasts" && f.broadcasts[parts[1]] == nil {
		reply(404, map[string]string{"detail": "Not found."})
		return
	}
	switch {
	case path == "/hosts":
		io.WriteString(w, f.hosts)
	case path == "/shows":
		io.WriteString(w, f.shows)
	case path == "/schedule/occurrences":
		io.WriteString(w, f.occurrences)
	case path == "/broadcasts" && r.Method == "POST":
		if f.taken != "" {
			reply(400, map[string]any{
				"start_date":   []string{"This scheduled airing already has a broadcast."},
				"broadcast_id": []string{f.taken},
			})
			return
		}
		id := "bcast_" + string(rune('A'+len(f.broadcasts)))
		in["id"], in["tracks"], in["recordings"] = id, []any{}, []any{}
		f.broadcasts[id] = in
		reply(201, in)
	case len(parts) == 2 && parts[0] == "broadcasts" && r.Method == "GET":
		reply(200, f.broadcasts[parts[1]])
	case len(parts) == 2 && parts[0] == "broadcasts" && r.Method == "PATCH":
		for k, v := range in {
			f.broadcasts[parts[1]][k] = v
		}
		reply(200, f.broadcasts[parts[1]])
	case len(parts) == 3 && parts[2] == "tracks" && r.Method == "PUT":
		f.broadcasts[parts[1]]["tracks"] = in["tracks"]
		reply(200, in["tracks"])
	case len(parts) == 3 && parts[2] == "recordings" && r.Method == "POST":
		if f.failAttach {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		b := f.broadcasts[parts[1]]
		for _, have := range b["recordings"].([]any) {
			if have.(map[string]any)["audio_file_id"] == in["audio_file_id"] {
				reply(400, map[string]any{"audio_file_id": []string{"This audio file is already attached to a recording."}})
				return
			}
		}
		b["recordings"] = append(b["recordings"].([]any), in)
		reply(201, in)
	case path == "/audio" && r.Method == "POST":
		id := "audio_" + string(rune('A'+len(f.completed)+len(f.uploads)))
		reply(201, map[string]any{
			"audio_file": map[string]any{"id": id},
			"upload_url": f.uploadBase + "/storage/" + id,
			"headers":    map[string]string{"Content-Type": "audio/mpeg"},
		})
	case len(parts) == 3 && parts[0] == "audio" && parts[2] == "complete" && r.Method == "POST":
		if f.uploads[parts[1]] == nil {
			reply(400, map[string]string{"detail": "No uploaded file found for this audio."})
			return
		}
		f.completed[parts[1]] = in
		reply(200, in)
	default:
		reply(404, map[string]string{"detail": "Not found."})
	}
}

func (f *fake) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fake) broadcast(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.broadcasts[id])
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

func (f *fake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

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

func TestSyncBuildsDJsAndSchedule(t *testing.T) {
	f := newFake(t)
	st := NewStation(NewClient(f.URL, "ktest", secret), "", quiet)
	if err := st.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots := st.Schedule.Slots(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	if len(slots) != 3 {
		t.Fatalf("%d slots", len(slots))
	}
	first := slots[0]
	if first.ID != "sched_1@20261118T200000Z" || first.Show.ID != "show_mg" || strings.Join(first.Hosts, ",") != "alice" ||
		first.End.Sub(first.Start) != time.Hour {
		t.Fatalf("first slot %+v", first)
	}
	if got := strings.Join(slots[1].Hosts, ","); got != "alice,bob" {
		t.Fatalf("hosts of a two-DJ show: %q", got)
	}
	if got := strings.Join(slots[1].HostNames, ","); got != "Alice,Bob" {
		t.Fatalf("their names: %q", got)
	}
	// A show nobody hosts is still a slot, which nobody can fill.
	if len(slots[2].Hosts) != 0 {
		t.Fatalf("automation slot has hosts %v", slots[2].Hosts)
	}
	shows := st.Schedule.ShowsFor("alice")
	if len(shows) != 2 {
		t.Fatalf("alice hosts %+v", shows)
	}
}

func TestStreamKeysAreCheckedByDigest(t *testing.T) {
	f := newFake(t)
	st := NewStation(NewClient(f.URL, "ktest", secret), "", quiet)
	if err := st.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// However the DJ types it.
	for _, typed := range []string{"AAAA-2222", "aaaa2222", " aaaa 2222 "} {
		id, err := st.DJs.Authenticate(ctx, "alice", typed, "10.0.0.1")
		if err != nil || id.Username != "alice" || id.Name != "Alice" {
			t.Fatalf("%q: %+v %v", typed, id, err)
		}
	}
	// Software that sends only a password is matched on the key alone.
	if id, err := st.DJs.Authenticate(ctx, "source", "BBBB-3333", "10.0.0.2"); err != nil || id.Username != "bob" {
		t.Fatalf("by key alone: %+v %v", id, err)
	}
	for _, bad := range [][2]string{{"alice", "BBBB-3333"}, {"alice", "nope"}, {"no-key", ""}, {"alice", digest("AAAA2222")}} {
		if _, err := st.DJs.Authenticate(ctx, bad[0], bad[1], "10.0.0.3"); err == nil {
			t.Fatalf("%v was let in", bad)
		}
	}
	// The refusal says which part was wrong, for the operator's log.
	for login, want := range map[[2]string]string{
		{"alice", "BBBB-3333"}:   "stream key is not this DJ's",
		{"wakebot", "AAAA-2222"}: `no DJ with a stream key has the username "wakebot" (2 DJs are known)`,
		{"source", "ZZZZ-9999"}:  "no DJ has the stream key",
	} {
		_, err := st.DJs.Authenticate(ctx, login[0], login[1], "10.0.1."+login[0])
		if !errors.Is(err, auth.ErrDenied) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v: %v", login, err)
		}
	}
}

func TestLastCopySurvivesAnOutageAndARestart(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "station.json")
	st := NewStation(NewClient(f.URL, "ktest", secret), path, quiet)
	if err := st.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.failing = true })
	if err := st.Sync(context.Background()); err == nil || Permanent(err) {
		t.Fatalf("an outage should be a passing failure: %v", err)
	}
	if _, err := st.DJs.Authenticate(context.Background(), "alice", "AAAA-2222", "10.0.0.1"); err != nil {
		t.Fatal("the DJ list was lost when a sync failed")
	}
	// A new process, with StreamLover still down.
	again := NewStation(NewClient(f.URL, "ktest", secret), path, quiet)
	if _, err := again.DJs.Authenticate(context.Background(), "alice", "AAAA-2222", "10.0.0.1"); err != nil {
		t.Fatal("the saved copy did not bring back the DJs")
	}
	if n := len(again.Schedule.Slots(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))); n != 3 {
		t.Fatalf("the saved copy has %d slots", n)
	}
}

func TestWrongAPIKeyIsPermanent(t *testing.T) {
	f := newFake(t)
	st := NewStation(NewClient(f.URL, "ktest", "slk_secret_wrong"), "", quiet)
	if err := st.Sync(context.Background()); !Permanent(err) {
		t.Fatalf("got %v", err)
	}
}

// reporting is a reporter running against a fake.
func reporting(t *testing.T, f *fake, statePath string) *Reporter {
	r := NewReporter(NewClient(f.URL, "ktest", secret), statePath, quiet)
	r.RetryDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
	return r
}

var (
	began   = time.Date(2026, 11, 18, 20, 3, 17, 0, time.UTC)
	morning = &schedule.Show{ID: "show_mg", Name: "Morning Glory"}
	aTurn   = controller.Turn{ID: "abc12345-1", SessionID: "abc12345", DJ: session.Identity{Username: "alice"}, Show: morning}
)

func event(kind string, turn controller.Turn, after time.Duration, title string) controller.Event {
	return controller.Event{Kind: kind, At: began.Add(after), Turn: turn, Title: title}
}

// recordingOf writes a file standing in for a turn's recording and returns
// its description and where it is.
func recordingOf(t *testing.T, turn controller.Turn, audio string) (recorder.Info, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "20261118-200317-alice-"+turn.ID+".mp3")
	if err := os.WriteFile(path, []byte(audio), 0o600); err != nil {
		t.Fatal(err)
	}
	return recorder.Info{
		File: filepath.Base(path), TurnID: turn.ID, ShowID: "show_mg", Seconds: 3420.5, Bitrate: 192000,
		EndedAt: time.Now(),
	}, path
}

func TestReportsAShowFromStartToEnd(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	r.OnEvent(event("track", aTurn, 40*time.Second, "Floating Points - Silhouettes"))
	r.OnEvent(event("track", aTurn, 5*time.Minute, "Just A Title"))
	r.OnEvent(event("track", aTurn, 5*time.Minute+time.Second, "Just A Title")) // announced again
	r.OnEvent(event("off_air", aTurn, 57*time.Minute, ""))
	waitFor(t, "the end to be set", func() bool {
		b := f.broadcast("bcast_A")
		return b != nil && b["end_date"] != nil
	})

	b := f.broadcast("bcast_A")
	// The real start, not the planned one, and private until published.
	if b["show_id"] != "show_mg" || b["start_date"] != "2026-11-18T20:03:17Z" || b["visibility"] != "private" {
		t.Fatalf("created as %v", b)
	}
	if b["end_date"] != "2026-11-18T21:00:17Z" {
		t.Fatalf("end %v", b["end_date"])
	}
	tracks, _ := json.Marshal(b["tracks"])
	want := `[{"album":"","artist":"Floating Points","name":"Silhouettes","offset_seconds":40},` +
		`{"album":"","artist":"","name":"Just A Title","offset_seconds":300}]`
	if string(tracks) != want {
		t.Fatalf("tracks %s", tracks)
	}
}

func TestDeliversARecordingThroughStreamLover(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })

	info, path := recordingOf(t, aTurn, "the audio of the show")
	if err := r.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	b := f.broadcast("bcast_A")
	recs := b["recordings"].([]any)
	if len(recs) != 1 {
		t.Fatalf("recordings %v", recs)
	}
	id, _ := recs[0].(map[string]any)["audio_file_id"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.uploads[id]) != "the audio of the show" {
		t.Fatalf("uploaded %q under %q", f.uploads[id], id)
	}
	// StreamLover is told what it cannot work out for itself. The bitrate
	// is in kbps there.
	if done := f.completed[id]; done["duration_seconds"] != 3420.5 || done["bitrate"] != float64(192) {
		t.Fatalf("completed with %v", done)
	}
	// This fake takes the upload itself, as a development StreamLover does,
	// and so is sent the key.
	if f.uploadAuth[id] != "Bearer "+secret {
		t.Fatalf("upload carried %q", f.uploadAuth[id])
	}
}

func TestTheAPIKeyIsNotSentToStorageOnAnotherHost(t *testing.T) {
	f := newFake(t)
	// In production the upload address is a presigned one on the storage
	// service, which would refuse a request that also carried credentials.
	var auth, got string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		auth, got = req.Header.Get("Authorization"), string(body)
		f.mu.Lock()
		f.uploads[strings.TrimPrefix(req.URL.Path, "/storage/")] = body
		f.mu.Unlock()
	}))
	defer storage.Close()
	f.set(func() { f.uploadBase = storage.URL })

	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })
	info, path := recordingOf(t, aTurn, "audio")
	if err := r.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	if got != "audio" || auth != "" {
		t.Fatalf("storage got %q with Authorization %q", got, auth)
	}
}

func TestARecordingWaitsForItsTurnToBeLogged(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	info, path := recordingOf(t, aTurn, "audio")
	// The turn has not been logged, as during an outage. Try again later.
	err := r.Deliver(context.Background(), info, path)
	if err == nil || errors.Is(err, recorder.ErrUndeliverable) {
		t.Fatalf("got %v, want an error worth retrying", err)
	}
	r.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })
	if err := r.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	// One that has waited a day will never be claimed.
	orphan, orphanPath := recordingOf(t, controller.Turn{ID: "ffffffff-9"}, "audio")
	orphan.EndedAt = time.Now().Add(-25 * time.Hour)
	if err := r.Deliver(context.Background(), orphan, orphanPath); !errors.Is(err, recorder.ErrUndeliverable) {
		t.Fatalf("got %v", err)
	}
}

func TestAFileOverTheLimitIsNotUploaded(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })
	info, path := recordingOf(t, aTurn, "")
	// A sparse file, so that nothing near this size is really written.
	if err := os.Truncate(path, MaxUploadBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(context.Background(), info, path); !errors.Is(err, recorder.ErrUndeliverable) {
		t.Fatalf("got %v", err)
	}
	for _, call := range f.sent() {
		if strings.HasPrefix(call, "POST /audio") {
			t.Fatalf("an upload was started: %s", call)
		}
	}
}

func TestAFailureAfterUploadingDoesNotUploadAgain(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })
	info, path := recordingOf(t, aTurn, "audio")

	f.set(func() { f.failAttach = true })
	if err := r.Deliver(context.Background(), info, path); err == nil || errors.Is(err, recorder.ErrUndeliverable) {
		t.Fatalf("got %v, want an error worth retrying", err)
	}
	f.set(func() { f.failAttach = false })
	if err := r.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	// And once more, as if the reply to that had been lost.
	if err := r.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	uploads := 0
	for _, call := range f.sent() {
		if strings.HasPrefix(call, "POST /audio {") {
			uploads++
		}
	}
	if uploads != 1 || len(f.broadcast("bcast_A")["recordings"].([]any)) != 1 {
		t.Fatalf("%d uploads, recordings %v", uploads, f.broadcast("bcast_A")["recordings"])
	}
}

func TestATurnWithNoShowIsNotReported(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	offRecord := controller.Turn{ID: "abc12345-2", DJ: session.Identity{Username: "carol"}, Forced: true}
	r.OnEvent(event("on_air", offRecord, 0, ""))
	r.OnEvent(event("track", offRecord, time.Minute, "A - B"))
	r.OnEvent(event("off_air", offRecord, time.Hour, ""))
	time.Sleep(60 * time.Millisecond)
	if calls := f.sent(); len(calls) != 0 {
		t.Fatalf("sent %v", calls)
	}
}

func TestCarriesOnWithABroadcastThatAlreadyExists(t *testing.T) {
	f := newFake(t)
	offset := 30
	f.set(func() {
		// The airing's broadcast is already there, public, with a track,
		// as when the DJ dropped out and has come back.
		f.broadcasts["bcast_Z"] = map[string]any{
			"id": "bcast_Z", "start_date": "2026-11-18T20:00:00Z", "visibility": "public",
			"tracks":     []any{map[string]any{"name": "Earlier", "artist": "Someone", "album": "", "offset_seconds": offset}},
			"recordings": []any{},
		}
		f.taken = "bcast_Z"
	})
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	r.OnEvent(event("track", aTurn, 2*time.Minute, "Burial - Archangel"))
	waitFor(t, "the tracklist to grow", func() bool { return len(f.broadcast("bcast_Z")["tracks"].([]any)) == 2 })

	b := f.broadcast("bcast_Z")
	// Nothing of it has been heard yet, so the station's choice applies.
	if b["visibility"] != "private" {
		t.Fatalf("visibility %v", b["visibility"])
	}
	// The earlier track is kept, and offsets count from the broadcast's
	// own start, which is three minutes before this turn's.
	second := b["tracks"].([]any)[1].(map[string]any)
	if second["name"] != "Archangel" || second["offset_seconds"] != float64(317) {
		t.Fatalf("second track %v", second)
	}
}

func TestAnAlreadyPublishedBroadcastKeepsItsVisibility(t *testing.T) {
	f := newFake(t)
	f.set(func() {
		f.broadcasts["bcast_Z"] = map[string]any{
			"id": "bcast_Z", "start_date": "2026-11-18T20:00:00Z", "visibility": "public",
			"tracks": []any{}, "recordings": []any{map[string]any{"id": "rec_1"}},
		}
		f.taken = "bcast_Z"
	})
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	r.OnEvent(event("off_air", aTurn, time.Hour, ""))
	waitFor(t, "the end to be set", func() bool { return f.broadcast("bcast_Z")["end_date"] != nil })
	if v := f.broadcast("bcast_Z")["visibility"]; v != "public" {
		t.Fatalf("visibility %v", v)
	}
}

func TestReportsWaitOutAnOutageInOrder(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.failing = true })
	r := reporting(t, f, "")
	r.OnEvent(event("on_air", aTurn, 0, ""))
	r.OnEvent(event("track", aTurn, time.Minute, "A - One"))
	r.OnEvent(event("off_air", aTurn, time.Hour, ""))
	time.Sleep(50 * time.Millisecond)
	if len(f.sent()) != 0 {
		t.Fatal("the fake recorded calls while it was down")
	}
	f.set(func() { f.failing = false })
	waitFor(t, "everything to arrive", func() bool { return len(f.sent()) == 3 })
	calls := f.sent()
	for i, prefix := range []string{"POST /broadcasts", "PUT /broadcasts/bcast_A/tracks", "PATCH /broadcasts/bcast_A"} {
		if !strings.HasPrefix(calls[i], prefix) {
			t.Fatalf("call %d was %q", i, calls[i])
		}
	}
}

func TestARefusedReportIsDroppedAndTheRestGoOn(t *testing.T) {
	f := newFake(t)
	r := reporting(t, f, "")
	gone := controller.Turn{ID: "x-1", DJ: session.Identity{Username: "alice"}, Show: morning}
	r.OnEvent(event("on_air", aTurn, 0, ""))
	// A recording for a turn whose broadcast has been deleted meanwhile.
	r.mu.Lock()
	r.turns[gone.ID] = &turn{BroadcastID: "bcast_missing", StartedAt: began}
	r.mu.Unlock()
	r.OnEvent(event("off_air", gone, time.Hour, ""))
	r.OnEvent(event("off_air", aTurn, time.Hour, ""))
	waitFor(t, "the later report to arrive", func() bool {
		b := f.broadcast("bcast_A")
		return b != nil && b["end_date"] != nil
	})
}

func TestARecordingFindsItsBroadcastAfterARestart(t *testing.T) {
	f := newFake(t)
	state := filepath.Join(t.TempDir(), "turns.json")
	first := reporting(t, f, state)
	first.OnEvent(event("on_air", aTurn, 0, ""))
	waitFor(t, "the broadcast to be logged", func() bool { return f.broadcast("bcast_A") != nil })

	// A new process delivers the recording that the crash left behind.
	second := reporting(t, f, state)
	info, path := recordingOf(t, aTurn, "audio")
	if err := second.Deliver(context.Background(), info, path); err != nil {
		t.Fatal(err)
	}
	if n := len(f.broadcast("bcast_A")["recordings"].([]any)); n != 1 {
		t.Fatalf("%d recordings", n)
	}
}
