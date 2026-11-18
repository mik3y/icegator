package controller

import (
	"time"

	"github.com/splatboard/icegator/internal/schedule"
	"github.com/splatboard/icegator/internal/session"
)

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
	// Shows are the shows this DJ hosts, when there is a schedule. An
	// operator putting the DJ on air off-schedule picks from these.
	Shows []schedule.Show `json:"shows,omitempty"`
}

// Status is a snapshot of the whole gate.
type Status struct {
	// DJs lists everyone connected. The DJ on air comes first, then those
	// who are ready, then those still connecting, then those held.
	DJs []DJ `json:"djs"`
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
	// HandoffTo is the session a handoff would put on air now, or "".
	HandoffTo string `json:"handoff_to"`

	// The rest is set only when there is a schedule.

	// Show is what the DJ on air is broadcasting, if anything.
	Show *schedule.Show `json:"show,omitempty"`
	// Forced reports that an operator put the DJ on air off-schedule.
	Forced bool `json:"forced,omitempty"`
	// OverrunSince is the end of the on-air DJ's slot, once it has passed.
	OverrunSince *time.Time `json:"overrun_since,omitempty"`
	// CutAt is when the overrunning DJ will be taken off, if they will be.
	CutAt *time.Time `json:"cut_at,omitempty"`
	// Current and Next are the slot in progress and the one after it.
	Current *schedule.Slot `json:"current,omitempty"`
	Next    *schedule.Slot `json:"next,omitempty"`
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
		if c.opt.Schedule != nil {
			d.Shows = c.opt.Schedule.ShowsFor(e.s.DJ.Username)
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
	if next, _ := c.handoffTargetPeek(); next != nil {
		st.HandoffTo = next.s.ID
	}
	var cur *schedule.Slot
	if c.opt.Schedule != nil {
		cur = c.currentSlot()
		st.Current, st.Next = cur, c.nextSlot()
	}
	if e := c.onAir; e != nil {
		st.OnAir, st.Link, st.LinkError, st.Title, st.Recording = e.s.ID, e.link, e.linkErr, e.title, e.recording
		st.Show, st.Forced = e.turn.Show, e.turn.Forced
		if e.link == "live" {
			t := e.liveSince
			st.LiveSince = &t
		}
		if slot := e.turn.Slot; slot != nil && !c.opt.Now().Before(slot.End) {
			end := slot.End
			st.OverrunSince = &end
		}
		if cut := c.cutAt(cur); !cut.IsZero() {
			st.CutAt = &cut
		}
	}
	return st
}

// handoffTargetPeek is handoffTarget without using up a turn number.
func (c *Controller) handoffTargetPeek() (*entry, Turn) {
	n := c.turns
	e, t := c.handoffTarget()
	c.turns = n
	return e, t
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
			if remoteIP(e.s.Remote) == ip {
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
