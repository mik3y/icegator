// Package controller owns the answer to "who is on air". Every connected
// DJ is registered here. The controller decides which one's frames go to
// the station and runs the upstream connection for them.
//
// It works in one of two ways. With no schedule it is a queue, in which the
// first ready DJ is on air and the rest wait their turn (queue.go). With a
// schedule, the DJ on air is whoever the programme says (scheduled.go).
// Everything else, including how a DJ's frames reach the station, is shared.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/schedule"
	"github.com/splatboard/icegator/internal/session"
	"github.com/splatboard/icegator/internal/upstream"
)

// State is where a DJ stands.
type State string

const (
	StateConnecting State = "connecting" // connected, not enough audio yet
	StateQueued     State = "queued"     // ready, and not on air
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

// Recording receives a copy of what one DJ's turn on air sent to the
// station.
type Recording interface {
	Write(frame []byte)
	Title(title string)
	// Gap marks a point where the station connection dropped.
	Gap()
	Close()
}

// Turn describes one stretch on air. A DJ who stays on across the boundary
// between two of their own shows has two turns.
type Turn struct {
	// ID is unique to the turn.
	ID string
	DJ session.Identity
	// SessionID is the DJ connection the turn belongs to.
	SessionID string
	// Show is what is being broadcast. It is nil when there is no
	// schedule, and when an operator put the DJ on air without naming one.
	Show *schedule.Show
	// Slot is the scheduled slot being filled, or nil if the turn is not
	// on the schedule.
	Slot *schedule.Slot
	// Forced reports that an operator put the DJ on air off-schedule.
	Forced bool

	// overrides is the slot whose ready DJ the operator passed over when
	// forcing this turn, if there was one. See settleScheduled.
	overrides string
}

// Event is something that happened on air, for whoever keeps the record.
type Event struct {
	Kind string // "on_air", "off_air" or "track"
	At   time.Time
	Turn Turn
	// Title is the track title, for "track" events.
	Title string
}

// Options configure a Controller.
type Options struct {
	Upstream Upstream
	// Account is the station login Icegator streams as.
	Account upstream.Credentials

	// Policy says whether a DJ with a claim to the air takes it by
	// themselves or waits for an operator. Default PolicyManual in a queue and PolicyAuto with a schedule.
	Policy Policy

	// KeepUpstream carries the station connection over from one DJ to the
	// next, so the station sees one unbroken source across a handoff
	// instead of a disconnect and a reconnect. See handover.go.
	KeepUpstream bool

	// Schedule switches the controller to following a programme. The
	// remaining fields in this group apply only then. See scheduled.go.
	Schedule schedule.Source
	// Overrun says how long a DJ may stay on air past the end of their
	// slot. The zero value allows none.
	Overrun Overrun

	// FallbackTitle gives the title to show when the DJ has sent none.
	FallbackTitle func(Turn) string
	// Record, if set, starts a recording for a turn.
	Record func(Turn) (Recording, error)
	// OnEvent, if set, is told when turns start and end and tracks change.
	OnEvent func(Event)
	// OnChange is called (without locks held) whenever state changes.
	OnChange func()
	Log      *slog.Logger
	// RetryDelay is the first wait after a failed upstream connect. It
	// doubles up to 8x. Default 1s.
	RetryDelay time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

type entry struct {
	s    *session.Session
	held bool

	// The rest is meaningful while this entry is on air, and is guarded
	// by Controller.mu.
	turn      Turn
	rebind    chan struct{} // tells the air goroutine that turn has changed
	link      string        // "connecting" | "live" | "retrying"
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
	turns   int
	parked  *parked // a station connection waiting for the next DJ
}

func New(opt Options) *Controller {
	if opt.Policy == "" {
		// A queue has no way to know that the DJ on air is finished, so
		// it waits to be told. A schedule says when each DJ is due.
		opt.Policy = PolicyManual
		if opt.Schedule != nil {
			opt.Policy = PolicyAuto
		}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.RetryDelay == 0 {
		opt.RetryDelay = time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.FallbackTitle == nil {
		opt.FallbackTitle = func(t Turn) string { return t.DJ.DisplayName() }
	}
	done := make(chan struct{})
	close(done)
	return &Controller{opt: opt, done: done}
}

// Run re-examines the schedule once a second until ctx is cancelled, so
// that slot boundaries take effect on time. Without a schedule there is
// nothing to do and it returns at once.
func (c *Controller) Run(ctx context.Context) {
	if c.opt.Schedule == nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.Tick()
		}
	}
}

// Tick re-examines who should be on air. Run calls it on a timer, and tests
// call it after moving the clock.
func (c *Controller) Tick() {
	c.mu.Lock()
	before := c.onAir
	c.settle()
	changed := c.onAir != before
	c.mu.Unlock()
	if changed {
		c.changed()
	}
}

// settle puts the right DJ on air for the current state of things. Call
// with mu held.
func (c *Controller) settle() {
	if c.opt.Schedule != nil {
		c.settleScheduled()
	} else {
		c.settleQueue()
	}
}

// Add registers a newly connected session. The caller must call Remove
// when the session ends.
func (c *Controller) Add(s *session.Session) {
	e := &entry{s: s, rebind: make(chan struct{}, 1)}
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
				c.switchTo(nil, Turn{})
			}
			break
		}
	}
	c.settle()
	c.mu.Unlock()
	c.opt.Log.Info("dj disconnected", "dj", s.DJ.Username, "session", s.ID)
	c.changed()
}

func (c *Controller) ready(e *entry) {
	c.mu.Lock()
	if c.opt.Schedule == nil {
		c.readyQueue(e)
	}
	c.settle()
	c.mu.Unlock()
	c.changed()
}

// Handoff takes the current DJ off air and puts on whoever is due next. It
// reports false, changing nothing, if nobody is.
func (c *Controller) Handoff() bool {
	c.mu.Lock()
	defer c.changed()
	defer c.mu.Unlock()
	next, turn := c.handoffTarget()
	if next == nil {
		return false
	}
	if c.onAir != nil && c.opt.Schedule == nil {
		// In a queue, the DJ who was handed off would otherwise be
		// first in line again.
		c.onAir.held = true
	}
	c.switchTo(next, turn)
	return true
}

// handoffTarget is who a handoff would put on air, with the turn they
// would get. Call with mu held.
func (c *Controller) handoffTarget() (*entry, Turn) {
	if c.opt.Schedule != nil {
		slot := c.currentSlot()
		if e := c.candidate(slot); e != nil {
			return e, c.scheduledTurn(e, slot)
		}
		return nil, Turn{}
	}
	if e := c.nextInQueue(); e != nil {
		return e, c.newTurn(e)
	}
	return nil, Turn{}
}

// Air puts a specific session on air now, displacing whoever is there.
// With a schedule this is how an operator puts a DJ on off-schedule, and
// showID may name one of that DJ's shows to broadcast as. An empty showID
// puts them on air with no show, which is neither recorded nor reported.
func (c *Controller) Air(sessionID, showID string) error {
	c.mu.Lock()
	defer c.changed()
	defer c.mu.Unlock()
	e := c.find(sessionID)
	switch {
	case e == nil:
		return errors.New("no such session")
	case !e.s.Ready():
		return errors.New("session is not sending audio yet")
	case c.onAir == e && showID == "":
		return nil
	}
	turn := c.newTurn(e)
	if c.opt.Schedule != nil {
		turn.Forced = true
		if showID != "" {
			for _, show := range c.opt.Schedule.ShowsFor(e.s.DJ.Username) {
				if show.ID == showID {
					turn.Show = &show
				}
			}
			if turn.Show == nil {
				return errors.New("that DJ does not host that show")
			}
		}
		// Putting this DJ on over a scheduled DJ who is ready is a
		// deliberate choice, and it stands for the rest of that slot.
		if slot := c.currentSlot(); slot != nil && !slot.Allows(e.s.DJ.Username) {
			if other := c.candidate(slot); other != nil || (c.onAir != nil && slot.Allows(c.onAir.s.DJ.Username)) {
				turn.overrides = slot.ID
			}
		}
	}
	if c.onAir != nil && c.onAir != e && c.opt.Schedule == nil {
		c.onAir.held = true
	}
	e.held = false
	c.switchTo(e, turn)
	// A DJ put on air during their own slot is simply on schedule.
	c.settle()
	return nil
}

// Pull takes a session off air (or out of contention) and holds it until
// an operator airs it again. Whoever is due next, if anyone, goes on.
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
		c.switchTo(nil, Turn{})
	}
	c.settle()
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

// Shutdown takes whoever is on air off and waits for their station
// connection and recording to be closed.
func (c *Controller) Shutdown() {
	c.mu.Lock()
	c.switchTo(nil, Turn{})
	done := c.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if p := c.claim(); p != nil {
		p.conn.Close()
	}
}

func (c *Controller) find(id string) *entry {
	for _, e := range c.entries {
		if e.s.ID == id {
			return e
		}
	}
	return nil
}

// newTurn makes a turn for e with no show attached. Call with mu held.
func (c *Controller) newTurn(e *entry) Turn {
	c.turns++
	return Turn{
		ID: e.s.ID + "-" + strconv.Itoa(c.turns), DJ: e.s.DJ, SessionID: e.s.ID,
	}
}

// switchTo changes who is on air. A nil entry means nobody. Call with mu
// held. The old upstream connection is closed before the new one opens,
// because the station allows only one source at a time.
func (c *Controller) switchTo(e *entry, turn Turn) {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.onAir != nil {
		c.opt.Log.Info("off air", "dj", c.onAir.s.DJ.Username, "session", c.onAir.s.ID)
		c.onAir.link, c.onAir.linkErr, c.onAir.title, c.onAir.recording = "", "", "", false
		c.onAir.turn = Turn{}
	}
	c.onAir = e
	if e == nil {
		return
	}
	e.turn = turn
	select { // a rebind meant for an earlier turn must not reach this one
	case <-e.rebind:
	default:
	}
	c.opt.Log.Info("on air", "dj", e.s.DJ.Username, "session", e.s.ID, "show", showName(turn.Show))
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

// retake gives the DJ on air a new turn without interrupting their audio.
// Call with mu held.
func (c *Controller) retake(e *entry, turn Turn) {
	e.turn = turn
	c.opt.Log.Info("new turn for the dj on air", "dj", e.s.DJ.Username, "show", showName(turn.Show))
	select {
	case e.rebind <- struct{}{}:
	default:
	}
}

func showName(s *schedule.Show) string {
	if s == nil {
		return ""
	}
	return s.Name
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

func (c *Controller) emit(kind string, turn Turn, title string) {
	if c.opt.OnEvent != nil {
		c.opt.OnEvent(Event{Kind: kind, At: c.opt.Now(), Turn: turn, Title: title})
	}
}

// air keeps e's frames flowing to the station until ctx is cancelled or the
// session ends, reconnecting if the station drops us.
func (c *Controller) air(ctx context.Context, e *entry) {
	creds := c.opt.Account
	delay := c.opt.RetryDelay
	log := c.opt.Log.With("dj", e.s.DJ.Username, "session", e.s.ID)

	// A turn begins the first time the station takes the stream, so that a
	// DJ the station refuses leaves no record behind.
	var turn Turn
	var rec Recording
	begun := false
	begin := func() {
		c.mu.Lock()
		turn = e.turn
		c.mu.Unlock()
		// With a schedule, a turn with no show is off the record.
		if c.opt.Record != nil && (c.opt.Schedule == nil || turn.Show != nil) {
			var err error
			if rec, err = c.opt.Record(turn); err != nil {
				log.Error("recording could not be started", "err", err)
				rec = nil
			}
		}
		c.mu.Lock()
		e.recording = rec != nil && c.onAir == e
		c.mu.Unlock()
		c.emit("on_air", turn, "")
		begun = true
	}
	end := func() {
		if rec != nil {
			rec.Close()
			rec = nil
		}
		c.emit("off_air", turn, "")
	}
	defer func() {
		if begun {
			end()
		}
	}()

	for ctx.Err() == nil {
		// Take over the previous DJ's station connection if they left one.
		// join is then the format of the stream already on it.
		var conn *upstream.Conn
		var join *mp3.Header
		var err error
		if p := c.claim(); p != nil && c.opt.KeepUpstream {
			conn, join = p.conn, &p.last
			log.Info("taking over the station connection")
		} else {
			if p != nil {
				p.conn.Close()
			}
			conn, err = c.opt.Upstream.Connect(ctx, creds, upstream.Info{Name: e.s.DJ.DisplayName()})
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
		if join == nil {
			log.Info("station connection live")
		}
		if !begun {
			begin()
		} else if rec != nil {
			rec.Gap()
		}
		c.setLink(e, "live", "")

		// About 13 seconds of slack at 26ms a frame before we are cut off.
		sub := e.s.Subscribe(512)
		var sent mp3.Header // the last frame written to this connection
		starting := true    // the DJ's stream is about to begin on conn
		result := pumpRebind
		for result == pumpRebind {
			result = c.pump(ctx, e, conn, creds, sub, rec, turn, starting, join, &sent)
			starting, join = false, nil
			if result == pumpRebind {
				// The same audio carries on under a new turn.
				end()
				begin()
				c.changed()
			}
		}
		sub.Close()
		switch result {
		case pumpDone:
			// This DJ is finished. Whoever is next can have the connection.
			if c.opt.KeepUpstream && sent.SampleRate != 0 {
				c.park(conn, sent)
			} else {
				conn.Close()
			}
			return
		case pumpFresh:
			conn.Close()
			log.Info("stream format differs from the previous dj's; opening a new station connection")
		default:
			conn.Close()
			log.Warn("upstream connection lost; reconnecting")
			c.setLink(e, "retrying", "connection lost")
		}
	}
}

type pumpResult int

const (
	pumpDone      pumpResult = iota // ctx cancelled or the session ended
	pumpReconnect                   // the station connection needs re-opening
	pumpRebind                      // the DJ on air has been given a new turn
	pumpFresh                       // this stream cannot follow the last one on the connection
)

// pump copies frames from sub to conn until something changes. starting
// says that the DJ's stream is beginning on conn, as opposed to carrying on
// under a new turn. A non-nil join means conn already carries another DJ's
// stream in that format (see handover.go). sent is kept pointing at the
// header of the last frame written.
func (c *Controller) pump(ctx context.Context, e *entry, conn *upstream.Conn, creds upstream.Credentials,
	sub *session.Sub, rec Recording, turn Turn, starting bool, join *mp3.Header, sent *mp3.Header) pumpResult {
	sendTitle := func(fromDJ string) {
		title := fromDJ
		if title == "" {
			title = c.opt.FallbackTitle(turn)
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
		if fromDJ != "" {
			c.emit("track", turn, fromDJ)
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
	// Neither a fresh station connection nor a new turn has a title yet.
	c.mu.Lock()
	e.title = ""
	c.mu.Unlock()
	sendTitle(e.s.Title())

	for {
		select {
		case <-ctx.Done():
			return pumpDone
		case <-conn.Closed:
			return pumpReconnect
		case <-e.rebind:
			return pumpRebind
		case t := <-sub.Title:
			sendTitle(t)
		case frame, ok := <-sub.Frames:
			if !ok {
				// Either the session ended, in which case Remove
				// will cancel us shortly, or we lagged. Reconnecting
				// is right for the second and harmless for the first.
				select {
				case <-ctx.Done():
					return pumpDone
				case <-time.After(50 * time.Millisecond):
					return pumpReconnect
				}
			}
			header, err := mp3.ParseHeader(frame)
			if err != nil {
				continue
			}
			if join != nil && !join.Joinable(header) {
				return pumpFresh
			}
			if starting {
				// The DJ has been streaming for a while, so this frame
				// may lean on ones the station was never sent. The lead
				// makes up for them.
				starting, join = false, nil
				for _, lead := range mp3.Lead(sub.Before, frame) {
					if err := conn.Write(lead); err != nil {
						return pumpReconnect
					}
					if rec != nil {
						rec.Write(lead)
					}
				}
			}
			if err := conn.Write(frame); err != nil {
				return pumpReconnect
			}
			*sent = header
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
