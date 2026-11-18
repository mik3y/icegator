package streamlover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/recorder"
)

// Reporter tells StreamLover what went to air. It logs a broadcast when a
// DJ goes on with a show, keeps the broadcast's tracklist up to date, sets
// its end, and uploads the recording, if recordings are being made.
//
// Everything is sent in the order it happened, by one worker, and a report
// that fails for a passing reason is retried until it gets through. What is
// waiting to be sent is held in memory, so a restart loses it. What is kept
// on disk is which broadcast each turn belongs to, which is what a
// recording left over from before a restart needs in order to find its
// broadcast.
type Reporter struct {
	client *Client
	log    *slog.Logger
	// Visibility is given to each broadcast this creates. Private keeps a
	// show's recording from listeners until someone publishes it.
	Visibility string
	// RetryDelay is the first wait after a failed report. It doubles up
	// to a couple of minutes. Default 2s.
	RetryDelay time.Duration

	path  string
	queue chan func(context.Context) error

	mu     sync.Mutex
	turns  map[string]*turn     // by turn id
	tracks map[string][]Track   // by broadcast id
	begun  map[string]time.Time // by broadcast id, when its audio began
}

// turn is what is remembered about one stretch on air.
type turn struct {
	BroadcastID string    `json:"broadcast_id"`
	StartedAt   time.Time `json:"started_at"`
	// AudioFileID is set once the turn's recording has been uploaded, so
	// that a failure after that point does not upload it a second time.
	AudioFileID string `json:"audio_file_id,omitempty"`
}

// keepTurnsFor is how long a turn is remembered. A recording is uploaded
// within moments of its turn ending, or at the next start after a crash.
const keepTurnsFor = 14 * 24 * time.Hour

// NewReporter makes a reporter. statePath is where the turn-to-broadcast
// record is kept, and may be empty to keep none.
func NewReporter(client *Client, statePath string, log *slog.Logger) *Reporter {
	if log == nil {
		log = slog.Default()
	}
	r := &Reporter{
		client: client, log: log, path: statePath, Visibility: "private", RetryDelay: 2 * time.Second,
		queue: make(chan func(context.Context) error, 4096),
		turns: map[string]*turn{}, tracks: map[string][]Track{}, begun: map[string]time.Time{},
	}
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			json.Unmarshal(b, &r.turns)
		}
	}
	return r
}

func (r *Reporter) saveTurns() {
	if r.path == "" {
		return
	}
	r.mu.Lock()
	for id, t := range r.turns {
		if time.Since(t.StartedAt) > keepTurnsFor {
			delete(r.turns, id)
		}
	}
	b, _ := json.Marshal(r.turns)
	r.mu.Unlock()
	os.MkdirAll(filepath.Dir(r.path), 0o700)
	tmp := r.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, r.path)
	}
}

func (r *Reporter) enqueue(what string, job func(context.Context) error) {
	select {
	case r.queue <- job:
	default:
		r.log.Error("too many reports are waiting for streamlover; dropping one", "report", what)
	}
}

// Run sends reports until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-r.queue:
			delay := r.RetryDelay
			for {
				err := job(ctx)
				if err == nil || ctx.Err() != nil {
					break
				}
				if Permanent(err) {
					r.log.Error("streamlover refused a report; it will not be sent again", "err", err)
					break
				}
				r.log.Warn("could not reach streamlover; the report will be retried", "err", err, "retry_in", delay)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				if delay < 2*time.Minute {
					delay *= 2
				}
			}
		}
	}
}

// OnEvent takes an event from the controller. A turn with no show is off
// the record and is not reported.
func (r *Reporter) OnEvent(e controller.Event) {
	if e.Turn.Show == nil {
		return
	}
	switch e.Kind {
	case "on_air":
		r.enqueue("on air", func(ctx context.Context) error { return r.startTurn(ctx, e) })
	case "track":
		r.enqueue("track", func(ctx context.Context) error { return r.addTrack(ctx, e) })
	case "off_air":
		r.enqueue("off air", func(ctx context.Context) error { return r.endTurn(ctx, e) })
	}
}

func (r *Reporter) broadcastOf(turnID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[turnID]
	if !ok {
		return "", false
	}
	return t.BroadcastID, true
}

func (r *Reporter) startTurn(ctx context.Context, e controller.Event) error {
	if _, known := r.broadcastOf(e.Turn.ID); known {
		return nil
	}
	b, err := r.client.CreateBroadcast(ctx, e.Turn.Show.ID, e.At, r.Visibility)
	var exists *ErrAiringHasBroadcast
	if errors.As(err, &exists) {
		// The scheduled airing already has a broadcast. The DJ dropped
		// and came back, or StreamLover opened one itself. Carry on with
		// that one, tracklist and all.
		if b, err = r.adopt(ctx, exists.BroadcastID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	r.mu.Lock()
	r.turns[e.Turn.ID] = &turn{BroadcastID: b.ID, StartedAt: e.At}
	if _, ok := r.begun[b.ID]; !ok {
		r.begun[b.ID] = b.StartDate
		r.tracks[b.ID] = b.Tracks
	}
	r.mu.Unlock()
	r.saveTurns()
	r.log.Info("broadcast logged", "show", e.Turn.Show.Name, "dj", e.Turn.DJ.Username, "broadcast", b.ID)
	return nil
}

// adopt reads a broadcast that already exists so that reporting can
// continue on it.
func (r *Reporter) adopt(ctx context.Context, id string) (*Broadcast, error) {
	b, err := r.client.Broadcast(ctx, id)
	if err != nil {
		return nil, err
	}
	// One that StreamLover opened is public, as its broadcasts are by
	// default. Nothing of it can have been heard until it has a
	// recording, so it is not too late to apply the station's choice.
	if r.Visibility != "" && b.Visibility != r.Visibility && len(b.Recordings) == 0 {
		if err := r.client.UpdateBroadcast(ctx, id, map[string]any{"visibility": r.Visibility}); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// splitTitle turns a title as DJ software sends it, which is "Artist -
// Track" by convention, into its two parts.
func splitTitle(title string) (artist, name string) {
	if a, n, ok := strings.Cut(title, " - "); ok && strings.TrimSpace(a) != "" && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(a), strings.TrimSpace(n)
	}
	return "", strings.TrimSpace(title)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (r *Reporter) addTrack(ctx context.Context, e controller.Event) error {
	id, ok := r.broadcastOf(e.Turn.ID)
	if !ok {
		return nil // its turn was never logged, so there is nothing to add to
	}
	artist, name := splitTitle(e.Title)
	if name == "" {
		return nil
	}
	artist, name = clip(artist, 256), clip(name, 256)

	r.mu.Lock()
	list := r.tracks[id]
	if n := len(list); n > 0 && list[n-1].Name == name && list[n-1].Artist == artist {
		r.mu.Unlock()
		return nil // the same song announced again, as after a reconnect
	}
	// Offsets count from the start of the broadcast, and StreamLover
	// requires each to be later than the one before.
	offset := max(0, int(e.At.Sub(r.begun[id]).Seconds()))
	if n := len(list); n > 0 && list[n-1].OffsetSeconds != nil && offset <= *list[n-1].OffsetSeconds {
		offset = *list[n-1].OffsetSeconds + 1
	}
	list = append(append([]Track(nil), list...), Track{Name: name, Artist: artist, OffsetSeconds: &offset})
	r.mu.Unlock()

	// The whole list is sent each time, which is how StreamLover's other
	// writers do it, and means a repeat changes nothing.
	if err := r.client.SetTracks(ctx, id, list); err != nil {
		return err
	}
	r.mu.Lock()
	r.tracks[id] = list
	r.mu.Unlock()
	return nil
}

func (r *Reporter) endTurn(ctx context.Context, e controller.Event) error {
	id, ok := r.broadcastOf(e.Turn.ID)
	if !ok {
		return nil
	}
	r.mu.Lock()
	begun := r.begun[id]
	r.mu.Unlock()
	if !begun.IsZero() && !e.At.After(begun) {
		return nil // an end that is not after the start would be refused
	}
	return r.client.UpdateBroadcast(ctx, id, map[string]any{"end_date": e.At.UTC().Format(time.RFC3339Nano)})
}

// waitForTurn is how long a recording waits for its turn to be logged
// before it is given up on. The turn is logged by the same worker that
// sends everything else, so during an outage it can be a while.
const waitForTurn = 24 * time.Hour

// Deliver uploads a finished recording through StreamLover and attaches it
// to the broadcast its turn belonged to. It is what the recorder calls, and
// it follows the recorder's contract: nil when the recording has arrived, an
// error wrapping recorder.ErrUndeliverable when it never will, and any other
// error to be tried again.
func (r *Reporter) Deliver(ctx context.Context, info recorder.Info, audioPath string) error {
	never := func(why string) error { return fmt.Errorf("%w: %s", recorder.ErrUndeliverable, why) }
	if info.TurnID == "" {
		return never("it does not say which turn it is a recording of")
	}
	r.mu.Lock()
	t, known := r.turns[info.TurnID]
	var broadcastID, audioFileID string
	if known {
		broadcastID, audioFileID = t.BroadcastID, t.AudioFileID
	}
	r.mu.Unlock()
	if !known {
		if time.Since(info.EndedAt) > waitForTurn {
			return never("its turn was never logged with streamlover")
		}
		return errors.New("its turn has not been logged with streamlover yet")
	}

	if audioFileID == "" {
		id, err := r.client.UploadAudio(ctx, audioPath, info.Seconds, info.Bitrate/1000)
		switch {
		case errors.Is(err, ErrTooLarge):
			return never(err.Error())
		case Permanent(err):
			return never(err.Error())
		case err != nil:
			return err
		}
		audioFileID = id
		r.mu.Lock()
		t.AudioFileID = id
		r.mu.Unlock()
		r.saveTurns()
	}
	err := r.client.AttachAudio(ctx, broadcastID, audioFileID)
	if Permanent(err) {
		return never(err.Error())
	}
	if err == nil {
		r.log.Info("recording uploaded to streamlover", "broadcast", broadcastID, "file", info.File)
	}
	return err
}
