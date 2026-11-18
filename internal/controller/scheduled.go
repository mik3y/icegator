package controller

import (
	"time"

	"github.com/splatboard/icegator/internal/schedule"
)

// With a schedule, the DJ on air is whoever the programme says, subject to
// a few rules.
//
// A DJ goes on air only inside a slot they are a host of, and never before
// it starts. When the slot ends they give way to the next slot's DJ as soon
// as that DJ is ready. If that DJ is not ready, or nothing is scheduled
// next, how long the DJ on air may carry on is the station's choice (see
// Overrun). An operator can override any of this by putting a DJ on air or
// pulling one off.
//
// A DJ put on air by an operator stays until they disconnect, are pulled,
// or a scheduled DJ becomes ready. The exception is a scheduled DJ who was
// already ready when the operator made that choice. They wait for a handoff
// or for the next slot, since the operator plainly meant to pass them over.

// Overrun says how long a DJ may stay on air past the end of their slot
// when nobody with a claim to the air is ready to take over. A scheduled DJ
// who is ready always takes over, whatever this says.
//
// The zero value is the strict rule, under which a DJ is taken off the
// moment their slot ends. A Limit allows that much longer. Unlimited lets
// them carry on until someone with a claim turns up or they disconnect.
type Overrun struct {
	Unlimited bool
	Limit     time.Duration
}

// lookAround is how far either side of now the schedule is consulted.
const lookAround = 24 * time.Hour

// currentSlot is the slot in progress now, or nil. If slots overlap, the
// one that started most recently wins. Call with mu held.
func (c *Controller) currentSlot() *schedule.Slot {
	now := c.opt.Now()
	var cur *schedule.Slot
	for _, slot := range c.opt.Schedule.Slots(now, now.Add(time.Second)) {
		if slot.Covers(now) {
			cur = &slot
		}
	}
	return cur
}

// candidate is the DJ who should take the slot. That is the first to have
// connected among its hosts who are ready and not held, leaving out whoever
// is on air already. Call with mu held.
func (c *Controller) candidate(slot *schedule.Slot) *entry {
	if slot == nil {
		return nil
	}
	for _, e := range c.entries {
		if e != c.onAir && !e.held && e.s.Ready() && slot.Allows(e.s.DJ.Username) {
			return e
		}
	}
	return nil
}

func (c *Controller) scheduledTurn(e *entry, slot *schedule.Slot) Turn {
	turn := c.newTurn(e)
	turn.Slot, turn.Show = slot, &slot.Show
	return turn
}

// cutAt is when the DJ on air will be taken off for overrunning, or zero
// if they will not be. It does not apply to a DJ an operator put on air,
// nor to one whose own show is the one now scheduled. cur is the slot in
// progress. Call with mu held.
func (c *Controller) cutAt(cur *schedule.Slot) time.Time {
	x := c.onAir
	if x == nil || x.turn.Forced || x.turn.Slot == nil || c.opt.Overrun.Unlimited {
		return time.Time{}
	}
	if cur != nil && cur.Allows(x.s.DJ.Username) {
		return time.Time{}
	}
	return x.turn.Slot.End.Add(c.opt.Overrun.Limit)
}

// settleScheduled applies the rules above. Call with mu held.
func (c *Controller) settleScheduled() {
	cur := c.currentSlot()
	x := c.onAir

	if x == nil {
		if e := c.candidate(cur); e != nil {
			c.switchTo(e, c.scheduledTurn(e, cur))
		}
		return
	}

	// The slot belongs to the DJ who is already on. If it is a different
	// slot from the one they went on for, a new turn starts without
	// touching the audio.
	if cur != nil && cur.Allows(x.s.DJ.Username) {
		if x.turn.Slot == nil || x.turn.Slot.ID != cur.ID {
			c.retake(x, c.scheduledTurn(x, cur))
		} else {
			x.turn.Slot = cur // pick up edits to the slot's times
		}
		return
	}

	// The DJ on air is overrunning, or was put on off-schedule.
	if e := c.candidate(cur); e != nil {
		// An operator who put this DJ on in place of one who was ready
		// has already decided against the switch, for this slot.
		overridden := x.turn.Forced && x.turn.overrides == cur.ID
		if c.opt.Policy == PolicyAuto && !overridden {
			c.switchTo(e, c.scheduledTurn(e, cur))
		}
		return // under the manual policy an operator hands off
	}
	if cut := c.cutAt(cur); !cut.IsZero() && !c.opt.Now().Before(cut) {
		c.opt.Log.Info("slot is over and no more overrun is allowed", "dj", x.s.DJ.Username)
		c.switchTo(nil, Turn{})
	}
}

// nextSlot is the first slot that starts after now. Call with mu held.
func (c *Controller) nextSlot() *schedule.Slot {
	now := c.opt.Now()
	for _, slot := range c.opt.Schedule.Slots(now, now.Add(lookAround)) {
		if slot.Start.After(now) {
			return &slot
		}
	}
	return nil
}
