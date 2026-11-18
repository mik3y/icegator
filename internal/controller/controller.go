// Package controller owns the answer to "who is on air". Every connected
// DJ is registered here; the controller decides which one's frames go to
// the station and runs the upstream connection for them.
//
// This file implements standalone (queue) mode, in which the first ready
// DJ is on air and the rest wait their turn.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/upstream"
)

// State is where a DJ stands.
type State string

const (
	StateConnecting State = "connecting" // connected, not enough audio yet
	StateQueued     State = "queued"     // ready, waiting their turn
	StateOnAir      State = "on_air"
	StateHeld       State = "held" // taken off air; needs an operator to return
)

// Policy says what happens when a DJ with a claim to the air is ready
// while another DJ is on it. In a queue, that is any DJ who connects. With
// a schedule, it is a DJ whose show is now on.
type Policy string

const (
	// PolicyAuto puts the DJ with the claim on air as soon as they are
	// ready.
	PolicyAuto Policy = "auto"
	// PolicyManual waits for an operator to hand off. In a queue the next
	// DJ also goes on when the current one disconnects.
	PolicyManual Policy = "manual"
)

// Upstream is the part of the station client the controller uses.
type Upstream interface {
	Connect(ctx context.Context, creds upstream.Credentials, info upstream.Info) (*upstream.Conn, error)
	SetTitle(ctx context.Context, creds upstream.Credentials, title string) error
}

// Options configure a Controller.
type Options struct {
	Upstream Upstream
	// Account is the station login used for DJs whose own credentials are
	// not forwarded.
	Account upstream.Credentials

	// Policy says whether a DJ with a claim to the air takes it by
	// themselves or waits for an operator. Default PolicyManual.
	Policy Policy
	// FallbackTitle gives the title to show when the DJ has sent none.
	FallbackTitle func(session.Identity) string
	// OnAccepted and OnRejected report how the station treated forwarded
	// credentials. Optional.
	OnAccepted func(upstream.Credentials)
	OnRejected func(upstream.Credentials)
	// Record, if set, starts a recording for a DJ who has gone on air.
	Record func(dj session.Identity, sessionID string) (Recording, error)
	// OnChange is called (without locks held) whenever state changes.
	OnChange func()
	Log      *slog.Logger
	// RetryDelay is the first wait after a failed upstream connect; it
	// doubles up to 8x. Default 1s.
	RetryDelay time.Duration
}

// Recording receives a copy of what one DJ's turn on air sent to the
// station.
type Recording interface {
	Write(frame []byte)
	Title(title string)
	// Gap marks a point where the station connection dropped.
	Gap()
	Close()
}

type entry struct {
	s    *session.Session
	held bool

	// Set while this entry is on air; guarded by Controller.mu.
	link      string // "connecting" | "live" | "retrying"
	linkErr   string
	liveSince time.Time
	title     string // last title sent upstream
	recording bool
}

// Controller is safe for concurrent use.
type Controller struct {
	opt Options

	mu      sync.Mutex
	entries []*entry // connection order
	onAir   *entry
	cancel  context.CancelFunc
	done    chan struct{} // closed when the current air goroutine has exited
}

func New(opt Options) *Controller {
	if opt.Policy == "" {
		opt.Policy = PolicyManual
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.RetryDelay == 0 {
		opt.RetryDelay = time.Second
	}
	if opt.FallbackTitle == nil {
		opt.FallbackTitle = func(id session.Identity) string { return id.DisplayName() }
	}
	done := make(chan struct{})
	close(done)
	return &Controller{opt: opt, done: done}
}

// Add registers a newly connected session. The caller must call Remove
// when the session ends.
func (c *Controller) Add(s *session.Session) {
	e := &entry{s: s}
	s.OnReady = func() { c.ready(e) }
	c.mu.Lock()
	c.entries = append(c.entries, e)
	c.mu.Unlock()
	c.opt.Log.Info("dj connected", "dj", s.DJ.Username, "session", s.ID, "remote", s.Remote)
	c.changed()
}

// Remove unregisters a session that has ended.
func (c *Controller) Remove(s *session.Session) {
	c.mu.Lock()
	for i, e := range c.entries {
		if e.s == s {
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
			if c.onAir == e {
				c.switchTo(nil)
			}
			break
		}
	}
	c.fill()
	c.mu.Unlock()
	c.opt.Log.Info("dj disconnected", "dj", s.DJ.Username, "session", s.ID)
	c.changed()
}

func (c *Controller) ready(e *entry) {
	c.mu.Lock()
	if c.opt.Policy == PolicyAuto && c.onAir != nil && c.onAir != e && e.s.Verified() && !e.held {
		c.onAir.held = true
		c.switchTo(e)
	}
	c.fill()
	c.mu.Unlock()
	c.changed()
}

// Handoff takes the current DJ off air and puts the next in the queue on.
// It reports false, changing nothing, if nobody is waiting.
func (c *Controller) Handoff() bool {
	c.mu.Lock()
	defer c.changed()
	defer c.mu.Unlock()
	next := c.next()
	if next == nil {
		return false
	}
	if c.onAir != nil {
		c.onAir.held = true
	}
	c.switchTo(next)
	return true
}

// Air puts a specific session on air now, displacing whoever is there.
func (c *Controller) Air(sessionID string) error {
	c.mu.Lock()
	defer c.changed()
	defer c.mu.Unlock()
	e := c.find(sessionID)
	switch {
	case e == nil:
		return errors.New("no such session")
	case !e.s.Ready():
		return errors.New("session is not sending audio yet")
	case c.onAir == e:
		return nil
	}
	if c.onAir != nil {
		c.onAir.held = true
	}
	e.held = false
	c.switchTo(e)
	return nil
}

// Pull takes a session off air (or out of the queue) and holds it until an
// operator airs it again. The next DJ in the queue, if any, goes on.
func (c *Controller) Pull(sessionID string) error {
	c.mu.Lock()
	defer c.changed()
	defer c.mu.Unlock()
	e := c.find(sessionID)
	if e == nil {
		return errors.New("no such session")
	}
	e.held = true
	if c.onAir == e {
		c.switchTo(nil)
	}
	c.fill()
	return nil
}

// Kick disconnects a session.
func (c *Controller) Kick(sessionID string) error {
	c.mu.Lock()
	e := c.find(sessionID)
	c.mu.Unlock()
	if e == nil {
		return errors.New("no such session")
	}
	e.s.Close()
	return nil
}

func (c *Controller) find(id string) *entry {
	for _, e := range c.entries {
		if e.s.ID == id {
			return e
		}
	}
	return nil
}

// next picks who should go on air after the current DJ: the longest-waiting
// ready DJ, preferring ones whose credentials are known to work.
func (c *Controller) next() *entry {
	var unverified *entry
	for _, e := range c.entries {
		if e == c.onAir || e.held || !e.s.Ready() {
			continue
		}
		if e.s.Verified() {
			return e
		}
		if unverified == nil {
			unverified = e
		}
	}
	return unverified
}

// fill puts the next DJ on air if nobody is. Call with mu held.
func (c *Controller) fill() {
	if c.onAir == nil {
		if e := c.next(); e != nil {
			c.switchTo(e)
		}
	}
}

// switchTo changes who is on air; nil means nobody. Call with mu held. The
// old upstream connection is closed before the new one opens, because the
// station allows only one source at a time.
func (c *Controller) switchTo(e *entry) {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.onAir != nil {
		c.opt.Log.Info("off air", "dj", c.onAir.s.DJ.Username, "session", c.onAir.s.ID)
		c.onAir.link, c.onAir.linkErr, c.onAir.title, c.onAir.recording = "", "", "", false
	}
	c.onAir = e
	if e == nil {
		return
	}
	c.opt.Log.Info("on air", "dj", e.s.DJ.Username, "session", e.s.ID)
	ctx, cancel := context.WithCancel(context.Background())
	prev, done := c.done, make(chan struct{})
	c.cancel, c.done = cancel, done
	e.link = "connecting"
	go func() {
		defer close(done)
		<-prev
		c.air(ctx, e)
	}()
}

func (c *Controller) creds(e *entry) upstream.Credentials {
	if e.s.DJ.Upstream != nil {
		return *e.s.DJ.Upstream
	}
	return c.opt.Account
}

func (c *Controller) setLink(e *entry, link, errText string) {
	c.mu.Lock()
	if c.onAir == e {
		e.link, e.linkErr = link, errText
		if link == "live" {
			e.liveSince = time.Now()
		}
	}
	c.mu.Unlock()
	c.changed()
}

// air keeps e's frames flowing to the station until ctx is cancelled or the
// session ends, reconnecting if the station drops us.
func (c *Controller) air(ctx context.Context, e *entry) {
	creds := c.creds(e)
	forwarded := e.s.DJ.Upstream != nil
	delay := c.opt.RetryDelay
	log := c.opt.Log.With("dj", e.s.DJ.Username, "session", e.s.ID)
	var rec Recording
	defer func() {
		if rec != nil {
			rec.Close()
		}
	}()

	for ctx.Err() == nil {
		conn, err := c.opt.Upstream.Connect(ctx, creds, upstream.Info{Name: e.s.DJ.DisplayName()})
		if errors.Is(err, upstream.ErrUnauthorized) && forwarded {
			// The DJ's own password is wrong, so retrying is
			// pointless. Drop them so the next in the queue gets
			// the air.
			log.Warn("station rejected dj's credentials; disconnecting them")
			if c.opt.OnRejected != nil {
				c.opt.OnRejected(creds)
			}
			e.s.Close()
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("upstream connect failed", "err", err, "retry_in", delay)
			c.setLink(e, "retrying", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 8*c.opt.RetryDelay {
				delay *= 2
			}
			continue
		}
		delay = c.opt.RetryDelay
		if forwarded {
			e.s.SetVerified()
			if c.opt.OnAccepted != nil {
				c.opt.OnAccepted(creds)
			}
		}
		log.Info("station connection live")
		switch {
		case rec != nil:
			rec.Gap()
		case c.opt.Record != nil:
			// Started only now, so that a DJ the station refuses
			// leaves no recording behind.
			if rec, err = c.opt.Record(e.s.DJ, e.s.ID); err != nil {
				log.Error("recording could not be started", "err", err)
				rec = nil
			}
			c.mu.Lock()
			e.recording = rec != nil && c.onAir == e
			c.mu.Unlock()
		}
		c.setLink(e, "live", "")
		ended := c.pump(ctx, e, conn, creds, rec)
		conn.Close()
		if ended {
			return
		}
		log.Warn("upstream connection lost; reconnecting")
		c.setLink(e, "retrying", "connection lost")
	}
}

// pump copies frames to conn. It returns true when there is nothing more
// to do (ctx cancelled or the session ended), false to reconnect.
func (c *Controller) pump(ctx context.Context, e *entry, conn *upstream.Conn, creds upstream.Credentials, rec Recording) bool {
	// About 13 seconds of slack at 26ms a frame before we are cut off.
	sub := e.s.Subscribe(512)
	defer sub.Close()

	sendTitle := func(title string) {
		if title == "" {
			title = c.opt.FallbackTitle(e.s.DJ)
		}
		c.mu.Lock()
		same := e.title == title
		e.title = title
		c.mu.Unlock()
		if same {
			return
		}
		if rec != nil {
			rec.Title(title)
		}
		go func() {
			tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := c.opt.Upstream.SetTitle(tctx, creds, title); err != nil && ctx.Err() == nil {
				c.opt.Log.Warn("title update failed", "dj", e.s.DJ.Username, "err", err)
			}
			c.changed()
		}()
	}
	c.mu.Lock()
	e.title = "" // a fresh source connection has no title yet
	c.mu.Unlock()
	sendTitle(e.s.Title())

	for {
		select {
		case <-ctx.Done():
			return true
		case <-conn.Closed:
			return false
		case t := <-sub.Title:
			sendTitle(t)
		case frame, ok := <-sub.Frames:
			if !ok {
				// Either the session ended (Remove will cancel us
				// shortly) or we lagged; reconnecting is right for
				// the second and harmless for the first.
				select {
				case <-ctx.Done():
					return true
				case <-time.After(50 * time.Millisecond):
					return false
				}
			}
			if err := conn.Write(frame); err != nil {
				return false
			}
			if rec != nil {
				rec.Write(frame)
			}
		}
	}
}

func (c *Controller) changed() {
	if c.opt.OnChange != nil {
		c.opt.OnChange()
	}
}

// DJ is one connected DJ as shown to operators.
type DJ struct {
	SessionID   string        `json:"session_id"`
	Username    string        `json:"username"`
	Name        string        `json:"name"`
	State       State         `json:"state"`
	Verified    bool          `json:"verified"`
	Remote      string        `json:"remote"`
	UserAgent   string        `json:"user_agent"`
	ConnectedAt time.Time     `json:"connected_at"`
	Title       string        `json:"title"`
	Stats       session.Stats `json:"stats"`
	// Level is the latest meter reading, or nil if there is none yet.
	Level *session.Level `json:"level"`
}

// Status is a snapshot of the whole gate.
type Status struct {
	DJs []DJ `json:"djs"` // on air first, then queue order, then held
	// OnAir is the session on air, or "".
	OnAir string `json:"on_air"`
	// Link is the state of the station connection while someone is on
	// air. It is "connecting", "live" or "retrying".
	Link      string     `json:"link,omitempty"`
	LinkError string     `json:"link_error,omitempty"`
	LiveSince *time.Time `json:"live_since,omitempty"`
	// Title is what the station is currently showing for the live DJ.
	Title string `json:"title,omitempty"`
	// Recording reports whether the live DJ's turn is being recorded.
	Recording bool `json:"recording"`
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	// An empty list rather than a null one, so that clients can always
	// iterate over it.
	st := Status{DJs: []DJ{}}
	rank := func(s State) int {
		return map[State]int{StateOnAir: 0, StateQueued: 1, StateConnecting: 2, StateHeld: 3}[s]
	}
	for _, e := range c.entries {
		d := DJ{
			SessionID: e.s.ID, Username: e.s.DJ.Username, Name: e.s.DJ.DisplayName(),
			Verified: e.s.Verified(), Remote: e.s.Remote, UserAgent: e.s.UserAgent,
			ConnectedAt: e.s.ConnectedAt, Title: e.s.Title(), Stats: e.s.Stats(), Level: e.s.Level.Load(),
		}
		switch {
		case e == c.onAir:
			d.State = StateOnAir
		case e.held:
			d.State = StateHeld
		case e.s.Ready():
			d.State = StateQueued
		default:
			d.State = StateConnecting
		}
		// Stable insertion by rank keeps connection order within a state.
		i := len(st.DJs)
		for i > 0 && rank(st.DJs[i-1].State) > rank(d.State) {
			i--
		}
		st.DJs = append(st.DJs, DJ{})
		copy(st.DJs[i+1:], st.DJs[i:])
		st.DJs[i] = d
	}
	if e := c.onAir; e != nil {
		st.OnAir, st.Link, st.LinkError, st.Title, st.Recording = e.s.ID, e.link, e.linkErr, e.title, e.recording
		if e.link == "live" {
			t := e.liveSince
			st.LiveSince = &t
		}
	}
	return st
}

// OnAirTitle is the title the station is showing for the DJ on air through
// Icegator, and whether anyone is.
func (c *Controller) OnAirTitle() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onAir == nil || c.onAir.link != "live" {
		return "", false
	}
	return c.onAir.title, true
}

// Unverified counts connected sessions whose credentials are not known to
// work, in total and from one address.
func (c *Controller) Unverified(ip string) (total, fromIP int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if !e.s.Verified() {
			total++
			if host := remoteIP(e.s.Remote); host == ip {
				fromIP++
			}
		}
	}
	return
}

// Find returns a connected session whose identity satisfies match,
// preferring the one on air.
func (c *Controller) Find(match func(session.Identity) bool) *session.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onAir != nil && match(c.onAir.s.DJ) {
		return c.onAir.s
	}
	for _, e := range c.entries {
		if match(e.s.DJ) {
			return e.s
		}
	}
	return nil
}

func remoteIP(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

// Levels returns the latest meter reading for every session that has one.
func (c *Controller) Levels() map[string]*session.Level {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := make(map[string]*session.Level, len(c.entries))
	for _, e := range c.entries {
		if l := e.s.Level.Load(); l != nil {
			m[e.s.ID] = l
		}
	}
	return m
}

// Shutdown takes whoever is on air off and waits for their station
// connection and recording to be closed.
func (c *Controller) Shutdown() {
	c.mu.Lock()
	c.switchTo(nil)
	done := c.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
