// Package recorder keeps a copy of what goes to air.
//
// Each turn a DJ has on air becomes one MP3 file, made of exactly the frames
// that were sent to the station. The file is written to a local spool
// directory, and when the turn ends it is handed to whatever is set to
// deliver it somewhere. Once delivered it is deleted. Recording therefore
// never depends on the destination being reachable at air time. Anything
// still in the spool at startup is delivered then, which covers both a
// failed delivery and a crash in the middle of a show.
package recorder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/session"
)

// ErrUndeliverable is for a Deliver function to wrap when a recording can
// never be delivered, so that trying again is pointless. The recording is
// then set aside in the spool's "undelivered" directory, not deleted.
var ErrUndeliverable = errors.New("recording cannot be delivered")

// Info describes a finished recording. It is kept beside the audio in the
// spool as a JSON file with the same name, and its presence is what marks a
// recording as complete.
type Info struct {
	// File is the name of the audio file in the spool.
	File      string `json:"file"`
	DJ        string `json:"dj"`
	DJName    string `json:"dj_name"`
	SessionID string `json:"session_id"`
	// TurnID names the stretch on air this is a recording of. A DJ who
	// stays connected across two shows has one session and two turns.
	TurnID string `json:"turn_id"`
	// ShowID and Show say what was being broadcast, when that is known.
	ShowID    string    `json:"show_id,omitempty"`
	Show      string    `json:"show,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Seconds is the length of the audio in the file.
	Seconds    float64 `json:"seconds"`
	Bytes      int64   `json:"bytes"`
	Bitrate    int     `json:"bitrate"`
	SampleRate int     `json:"sample_rate"`
	// Titles are the track titles sent to the station, each with its
	// position in the file.
	Titles []Mark `json:"titles"`
	// Gaps are the places where the station connection dropped and audio
	// that the DJ sent did not go to air. The file simply continues, so a
	// gap marks a jump rather than a silence.
	Gaps []Mark `json:"gaps"`
	// Recovered is set when the file was found unfinished after a crash,
	// in which case the times and marks above are reconstructed or lost.
	Recovered bool `json:"recovered,omitempty"`
}

// Mark is a point in a recording.
type Mark struct {
	OffsetSeconds float64 `json:"offset_seconds"`
	Title         string  `json:"title,omitempty"`
}

// Options configure a Recorder.
type Options struct {
	// Dir is the spool directory.
	Dir string
	// Deliver sends a finished recording wherever it is going. A nil
	// return means it has arrived and the local copy can go. An error that
	// wraps ErrUndeliverable means it never will, and any other error means
	// try again later.
	Deliver func(ctx context.Context, info Info, audioPath string) error
	// MinDuration discards anything shorter, such as a test connection.
	// Default 30s.
	MinDuration time.Duration
	Log         *slog.Logger
	// RetryDelay is the wait after a failed delivery. Default 1 minute.
	RetryDelay time.Duration
}

// Recorder makes recordings and sees them delivered.
type Recorder struct {
	opt   Options
	queue chan string // info file paths ready to deliver
}

func New(opt Options) (*Recorder, error) {
	if opt.MinDuration == 0 {
		opt.MinDuration = 30 * time.Second
	}
	if opt.RetryDelay == 0 {
		opt.RetryDelay = time.Minute
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	r := &Recorder{opt: opt, queue: make(chan string, 1024)}
	// This has to happen before any recording can start, because an
	// unfinished recording and one in progress look the same on disk.
	r.recover()
	return r, nil
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Recording is one file being written.
type Recording struct {
	r    *Recorder
	path string
	info Info

	mu       sync.Mutex
	f        *os.File
	w        *bufio.Writer
	audio    time.Duration
	lastSync time.Duration
	closed   bool
}

// Subject says what a recording is of.
type Subject struct {
	DJ        session.Identity
	SessionID string
	TurnID    string
	ShowID    string
	Show      string
}

// Start begins a recording for a DJ who has just gone on air.
func (r *Recorder) Start(sub Subject) (*Recording, error) {
	now := time.Now().UTC()
	dj := sub.DJ
	slug := strings.Trim(unsafeName.ReplaceAllString(dj.Username, "-"), "-")
	if slug == "" {
		slug = "dj"
	}
	// The turn goes in the name so that a recording cut off by a crash
	// can still be matched to its turn afterwards.
	id := sub.TurnID
	if id == "" {
		id = sub.SessionID
	}
	name := fmt.Sprintf("%s-%s-%s.mp3", now.Format("20060102-150405"), slug, id)
	path := filepath.Join(r.opt.Dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &Recording{
		r: r, path: path, f: f, w: bufio.NewWriterSize(f, 64*1024),
		info: Info{
			File: name, DJ: dj.Username, DJName: dj.DisplayName(),
			SessionID: sub.SessionID, TurnID: sub.TurnID, ShowID: sub.ShowID, Show: sub.Show,
			StartedAt: now,
		},
	}, nil
}

// Write appends one frame that was sent to the station.
func (rec *Recording) Write(frame []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		return
	}
	if h, err := mp3.ParseHeader(frame); err == nil {
		rec.audio += h.Duration()
		rec.info.Bitrate, rec.info.SampleRate = h.Bitrate, h.SampleRate
	}
	rec.w.Write(frame)
	rec.info.Bytes += int64(len(frame))
	// Push to disk every few seconds so that a crash loses little.
	if rec.audio-rec.lastSync > 5*time.Second {
		rec.w.Flush()
		rec.lastSync = rec.audio
	}
}

// Title notes that the station was sent a new track title.
func (rec *Recording) Title(title string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// A title is re-sent whenever the station connection is re-opened.
	if n := len(rec.info.Titles); n > 0 && rec.info.Titles[n-1].Title == title {
		return
	}
	rec.info.Titles = append(rec.info.Titles, Mark{OffsetSeconds: rec.audio.Seconds(), Title: title})
}

// Gap notes that the station connection dropped at this point.
func (rec *Recording) Gap() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.audio == 0 {
		return // nothing has been recorded yet, so there is nothing to break
	}
	rec.info.Gaps = append(rec.info.Gaps, Mark{OffsetSeconds: rec.audio.Seconds()})
}

// Close finishes the file and queues it for delivery.
func (rec *Recording) Close() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		return
	}
	rec.closed = true
	rec.w.Flush()
	rec.f.Close()
	rec.info.EndedAt = time.Now().UTC()
	rec.info.Seconds = rec.audio.Seconds()
	rec.r.finish(rec.path, rec.info)
}

// finish writes the info file, which is what marks a recording as complete,
// and queues it for delivery. Recordings that are too short are deleted
// instead.
func (r *Recorder) finish(path string, info Info) {
	if info.Seconds < r.opt.MinDuration.Seconds() {
		r.opt.Log.Info("recording discarded as too short", "dj", info.DJ, "seconds", int(info.Seconds))
		os.Remove(path)
		return
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	infoPath := strings.TrimSuffix(path, ".mp3") + ".json"
	tmp := infoPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		r.opt.Log.Error("recording info not written", "path", infoPath, "err", err)
		return
	}
	os.Rename(tmp, infoPath)
	r.opt.Log.Info("recording finished", "dj", info.DJ, "seconds", int(info.Seconds), "file", info.File)
	select {
	case r.queue <- infoPath:
	default: // Run picks it up from the spool on its next pass
	}
}

// Run delivers finished recordings until ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	retry := time.NewTicker(r.opt.RetryDelay)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case infoPath := <-r.queue:
			r.deliver(ctx, infoPath)
		case <-retry.C:
			// Anything still here failed earlier, or missed the queue.
			pending, _ := filepath.Glob(filepath.Join(r.opt.Dir, "*.json"))
			for _, infoPath := range pending {
				r.deliver(ctx, infoPath)
			}
		}
	}
}

// recover finishes recordings that were cut off by a crash, recognisable
// as audio files with no info file, and queues everything in the spool.
func (r *Recorder) recover() {
	audio, _ := filepath.Glob(filepath.Join(r.opt.Dir, "*.mp3"))
	for _, path := range audio {
		infoPath := strings.TrimSuffix(path, ".mp3") + ".json"
		if _, err := os.Stat(infoPath); err == nil {
			continue
		}
		info, err := r.reconstruct(path)
		if err != nil {
			r.opt.Log.Warn("unfinished recording could not be recovered", "path", path, "err", err)
			continue
		}
		r.opt.Log.Info("recovered an unfinished recording", "path", path, "seconds", int(info.Seconds))
		r.finish(path, info)
	}
	pending, _ := filepath.Glob(filepath.Join(r.opt.Dir, "*.json"))
	for _, infoPath := range pending {
		select {
		case r.queue <- infoPath:
		default:
		}
	}
}

// The last part is a session id (eight hex digits), or a turn id, which is a
// session id with a counter after it.
var spoolName = regexp.MustCompile(`^(\d{8}-\d{6})-(.+)-([0-9a-f]{8})(-\d+)?\.mp3$`)

func (r *Recorder) reconstruct(path string) (Info, error) {
	m := spoolName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return Info{}, errors.New("unrecognised file name")
	}
	started, err := time.Parse("20060102-150405", m[1])
	if err != nil {
		return Info{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	info := Info{
		File: filepath.Base(path), DJ: m[2], DJName: m[2], SessionID: m[3],
		StartedAt: started, Recovered: true,
	}
	if m[4] != "" {
		info.TurnID = m[3] + m[4]
	}
	var audio time.Duration
	fr := mp3.NewReader(f)
	for {
		frame, err := fr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Info{}, err
		}
		audio += frame.Header.Duration()
		info.Bytes += int64(len(frame.Data))
		info.Bitrate, info.SampleRate = frame.Header.Bitrate, frame.Header.SampleRate
	}
	info.Seconds = audio.Seconds()
	info.EndedAt = started.Add(audio)
	return info, nil
}

func (r *Recorder) deliver(ctx context.Context, infoPath string) {
	b, err := os.ReadFile(infoPath)
	if err != nil {
		return // already delivered and removed by an earlier pass
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		r.opt.Log.Error("recording info unreadable", "path", infoPath, "err", err)
		return
	}
	audioPath := strings.TrimSuffix(infoPath, ".json") + ".mp3"
	if _, err := os.Stat(audioPath); os.IsNotExist(err) {
		r.opt.Log.Error("recording audio is missing from the spool, so its info is being dropped", "file", info.File)
		os.Remove(infoPath)
		return
	}
	err = r.opt.Deliver(ctx, info, audioPath)
	switch {
	case err == nil:
		r.opt.Log.Info("recording delivered", "file", info.File, "seconds", int(info.Seconds))
		os.Remove(audioPath)
		os.Remove(infoPath)
	case errors.Is(err, ErrUndeliverable):
		// Kept, out of the way, in case someone wants to rescue it by hand.
		aside := filepath.Join(r.opt.Dir, "undelivered")
		os.MkdirAll(aside, 0o700)
		os.Rename(audioPath, filepath.Join(aside, filepath.Base(audioPath)))
		os.Rename(infoPath, filepath.Join(aside, filepath.Base(infoPath)))
		r.opt.Log.Error("recording could not be delivered and has been set aside", "file", info.File, "dir", aside, "err", err)
	case ctx.Err() == nil:
		r.opt.Log.Warn("recording delivery failed and will be retried", "file", info.File, "err", err)
	}
}
