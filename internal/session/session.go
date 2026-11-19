// Package session models one connected DJ. It reads their MP3 stream frame
// by frame, keeps running statistics, and fans the frames out to whoever is
// subscribed, such as the upstream connection while the DJ is on air.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/splatboard/icegator/internal/mp3"
)

// Identity is who a session belongs to, as established at connect time.
type Identity struct {
	Username string
	Name     string // display name; falls back to Username
}

func (i Identity) DisplayName() string {
	if i.Name != "" {
		return i.Name
	}
	return i.Username
}

// Session is one DJ connection.
type Session struct {
	ID          string
	DJ          Identity
	Remote      string
	UserAgent   string
	ConnectedAt time.Time

	// ReadyAfter is how much valid audio must arrive before the session
	// is considered ready to go on air.
	ReadyAfter time.Duration
	// OnReady is called once, from the reading goroutine.
	OnReady func()

	// Level is the most recent meter reading, or nil before the first.
	Level atomic.Pointer[Level]

	closer io.Closer

	mu       sync.Mutex
	header   mp3.Header
	frames   int64
	bytes    int64
	audio    time.Duration
	lastData time.Time
	ready    bool
	title    string
	subs     map[*Sub]struct{}
	recent   [][]byte // the last few frames read
	ended    bool
}

// New creates a session. closer is closed by Close to disconnect the DJ.
func New(dj Identity, remote, userAgent string, closer io.Closer) *Session {
	b := make([]byte, 4)
	rand.Read(b)
	return &Session{
		ID:          hex.EncodeToString(b),
		DJ:          dj,
		Remote:      remote,
		UserAgent:   userAgent,
		ConnectedAt: time.Now(),
		ReadyAfter:  3 * time.Second,
		closer:      closer,
		subs:        map[*Sub]struct{}{},
	}
}

// Sub is a subscription to a session's frames and title changes.
type Sub struct {
	// Frames delivers each frame as its own slice. It is closed when the
	// session ends, or when the subscriber falls too far behind.
	Frames chan []byte
	// Title delivers the latest title; intermediate values may be skipped.
	Title chan string
	// Before holds the frames that came immediately before the first one
	// Frames will deliver, oldest first. A stream that starts part-way
	// through needs them to start cleanly (see mp3.Lead).
	Before [][]byte
	s      *Session
}

// Subscribe starts delivering frames from the next one received. buffer is
// how many frames the subscriber may lag before it is cut off.
func (s *Session) Subscribe(buffer int) *Sub {
	sub := &Sub{Frames: make(chan []byte, buffer), Title: make(chan string, 1), s: s}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub.Before = append([][]byte(nil), s.recent...)
	if s.ended {
		close(sub.Frames)
		return sub
	}
	s.subs[sub] = struct{}{}
	return sub
}

// Close stops delivery. It is safe to call more than once.
func (sub *Sub) Close() {
	sub.s.mu.Lock()
	defer sub.s.mu.Unlock()
	sub.s.drop(sub)
}

func (s *Session) drop(sub *Sub) {
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.Frames)
	}
}

// Run reads frames from r until it ends, and returns the reason. The caller
// owns removing the session from wherever it is registered afterwards.
func (s *Session) Run(r io.Reader) error {
	fr := mp3.NewReader(r)
	defer func() {
		s.mu.Lock()
		s.ended = true
		for sub := range s.subs {
			s.drop(sub)
		}
		s.mu.Unlock()
	}()
	for {
		f, err := fr.Next()
		if err != nil {
			return err
		}
		data := append([]byte(nil), f.Data...)

		s.mu.Lock()
		s.header = f.Header
		s.frames++
		s.bytes += int64(len(data))
		s.audio += f.Header.Duration()
		s.lastData = time.Now()
		// Frames are never modified once read, so keeping the last few
		// costs only the slice headers.
		if len(s.recent) == mp3.MaxLead {
			copy(s.recent, s.recent[1:])
			s.recent = s.recent[:mp3.MaxLead-1]
		}
		s.recent = append(s.recent, data)
		becameReady := !s.ready && s.audio >= s.ReadyAfter
		if becameReady {
			s.ready = true
		}
		for sub := range s.subs {
			select {
			case sub.Frames <- data:
			default:
				s.drop(sub) // too slow; it will notice and resubscribe
			}
		}
		s.mu.Unlock()

		if becameReady && s.OnReady != nil {
			s.OnReady()
		}
	}
}

// Close disconnects the DJ.
func (s *Session) Close() {
	if s.closer != nil {
		s.closer.Close()
	}
}

func (s *Session) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Title is the last track title the DJ sent, or "".
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// SetTitle records a title sent by the DJ and tells subscribers.
func (s *Session) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if title == s.title {
		return
	}
	s.title = title
	for sub := range s.subs {
		select {
		case <-sub.Title: // replace a title nobody has read yet
		default:
		}
		sub.Title <- title
	}
}

// Stats is a point-in-time view of a session's stream.
type Stats struct {
	Format     string        `json:"format"` // e.g. "MPEG-1 Layer 3"
	Bitrate    int           `json:"bitrate"`
	AvgBitrate int           `json:"avg_bitrate"`
	SampleRate int           `json:"sample_rate"`
	Channels   int           `json:"channels"`
	Frames     int64         `json:"frames"`
	Bytes      int64         `json:"bytes"`
	Audio      time.Duration `json:"audio_ns"`
	LastData   time.Time     `json:"last_data"`
}

func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Bitrate: s.header.Bitrate, SampleRate: s.header.SampleRate, Channels: s.header.Channels,
		Frames: s.frames, Bytes: s.bytes, Audio: s.audio, LastData: s.lastData,
	}
	if s.frames > 0 {
		st.Format = s.header.Version.String() + " Layer " + string(rune('0'+s.header.Layer))
	}
	if s.audio > 0 {
		st.AvgBitrate = int(float64(s.bytes) * 8 / s.audio.Seconds())
	}
	return st
}

// Level is a meter reading, in dBFS (0 is full scale; silence is -120).
type Level struct {
	// RMS and Peak are per channel (left, right) over the last quarter of
	// a second.
	RMS  [2]float64 `json:"rms"`
	Peak [2]float64 `json:"peak"`
	// Loudness is the RMS of both channels over the last few seconds; it
	// is what Verdict is judged on.
	Loudness float64 `json:"loudness"`
	// Verdict is "silent", "low", "good" or "high".
	Verdict string `json:"verdict"`
}

// Subscribers is the number of active subscriptions.
func (s *Session) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}
