package controller

// Without a schedule the controller is a queue. The first DJ with audio
// flowing is on air and later arrivals wait in the order they connected.

// readyQueue handles a DJ's audio starting to flow. Call with mu held.
func (c *Controller) readyQueue(e *entry) {
	if c.opt.Policy == PolicyAuto && c.onAir != nil && c.onAir != e && !e.held {
		c.onAir.held = true
		c.switchTo(e, c.newTurn(e))
	}
}

// nextInQueue picks who goes on air after the current DJ, which is the
// longest-waiting ready DJ. Call with mu held.
func (c *Controller) nextInQueue() *entry {
	for _, e := range c.entries {
		if e != c.onAir && !e.held && e.s.Ready() {
			return e
		}
	}
	return nil
}

// settleQueue puts the next DJ on air if nobody is. Call with mu held.
func (c *Controller) settleQueue() {
	if c.onAir == nil {
		if e := c.nextInQueue(); e != nil {
			c.switchTo(e, c.newTurn(e))
		}
	}
}
