package controller

// Without a schedule the controller is a queue. The first DJ with audio
// flowing is on air and later arrivals wait in the order they connected.

// readyQueue handles a DJ's audio starting to flow. Call with mu held.
func (c *Controller) readyQueue(e *entry) {
	if c.opt.Policy == PolicyAuto && c.onAir != nil && c.onAir != e && e.s.Verified() && !e.held {
		c.onAir.held = true
		c.switchTo(e, c.newTurn(e))
	}
}

// nextInQueue picks who goes on air after the current DJ. That is the
// longest-waiting ready DJ, preferring ones whose credentials are known to
// work. Call with mu held.
func (c *Controller) nextInQueue() *entry {
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

// settleQueue puts the next DJ on air if nobody is. Call with mu held.
func (c *Controller) settleQueue() {
	if c.onAir == nil {
		if e := c.nextInQueue(); e != nil {
			c.switchTo(e, c.newTurn(e))
		}
	}
}
