package controller

import (
	"time"

	"github.com/splatboard/icegator/internal/mp3"
	"github.com/splatboard/icegator/internal/upstream"
)

// Normally each DJ gets a station connection of their own, so a handoff is
// a disconnect followed by a connect, and the station plays its own
// programming in between. With KeepUpstream the outgoing DJ's connection is
// handed to the incoming one instead. The station sees a single source
// whose audio changes, with no gap for anything else to fill.
//
// Nothing is decoded or re-encoded to do this. The incoming DJ's frames
// simply follow the outgoing DJ's on the same connection. Two things keep
// the join from being audible as anything worse than a hard cut.
//
// The first is how the incoming stream starts. An MP3 frame may borrow
// audio data from the frames before it, and here the frames before it
// would be the other DJ's. So the incoming stream is preceded by a short
// lead of silent frames that carry the data its first frame reaches back
// for (see mp3.Lead). Every stream sent to the station starts this way,
// handed-over or not.
//
// The second is format. A decoder set up for one sample rate and channel
// count cannot be relied on to cope with another mid-stream, so a DJ whose
// stream differs from the last one's gets a new connection after all.

// parkFor is how long an outgoing DJ's connection is kept open for the
// next DJ to claim. A handoff claims it within milliseconds. When nobody
// follows, this is how much later the station learns that it is on its own.
const parkFor = time.Second

// parked is a station connection between DJs.
type parked struct {
	conn  *upstream.Conn
	last  mp3.Header // format of the stream it has been carrying
	timer *time.Timer
}

// park leaves conn for the next DJ to claim, closing it if nobody does.
func (c *Controller) park(conn *upstream.Conn, last mp3.Header) {
	p := &parked{conn: conn, last: last}
	c.mu.Lock()
	old := c.parked
	c.parked = p
	p.timer = time.AfterFunc(parkFor, func() {
		c.mu.Lock()
		unclaimed := c.parked == p
		if unclaimed {
			c.parked = nil
		}
		c.mu.Unlock()
		if unclaimed {
			conn.Close()
		}
	})
	c.mu.Unlock()
	if old != nil {
		old.timer.Stop()
		old.conn.Close()
	}
}

// claim takes the parked connection, if there is one and the station has
// not closed it in the meantime.
func (c *Controller) claim() *parked {
	c.mu.Lock()
	p := c.parked
	c.parked = nil
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	p.timer.Stop()
	select {
	case <-p.conn.Closed:
		p.conn.Close()
		return nil
	default:
		return p
	}
}
