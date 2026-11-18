// Package schedule describes a station's programme as Icegator needs to see
// it, which is a list of time slots and who may be on air in each.
package schedule

import (
	"sort"
	"sync"
	"time"
)

// Show is a programme on the station.
type Show struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Slot is one scheduled airing of a show.
type Slot struct {
	ID    string    `json:"id"`
	Show  Show      `json:"show"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Hosts are the usernames of the DJs who may take this slot.
	Hosts []string `json:"hosts"`
	// HostNames are the same DJs' display names, in the same order, for
	// showing a slot whose DJs are not connected. Optional.
	HostNames []string `json:"host_names,omitempty"`
}

// Covers reports whether the slot is in progress at t.
func (s Slot) Covers(t time.Time) bool { return !t.Before(s.Start) && t.Before(s.End) }

// Allows reports whether the DJ may take this slot.
func (s Slot) Allows(username string) bool {
	for _, h := range s.Hosts {
		if h == username {
			return true
		}
	}
	return false
}

// Source supplies the schedule. Implementations answer from memory, since
// the controller asks every time it reconsiders who should be on air.
type Source interface {
	// Slots returns the slots that overlap [from, to), ordered by start.
	Slots(from, to time.Time) []Slot
	// ShowsFor lists the shows a DJ hosts, whether or not one is scheduled.
	ShowsFor(username string) []Show
}

// Static is a schedule held in memory. Tests use it, and a provider that
// syncs from elsewhere can keep its latest copy in one.
type Static struct {
	mu    sync.RWMutex
	slots []Slot
	shows map[string][]Show
}

// Set replaces the schedule. hosting maps a username to the shows that DJ
// hosts. Shows that appear in slots are added to their hosts automatically.
func (s *Static) Set(slots []Slot, hosting map[string][]Show) {
	slots = append([]Slot(nil), slots...)
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Start.Before(slots[j].Start) })
	shows := map[string][]Show{}
	add := func(user string, show Show) {
		for _, have := range shows[user] {
			if have.ID == show.ID {
				return
			}
		}
		shows[user] = append(shows[user], show)
	}
	for user, list := range hosting {
		for _, show := range list {
			add(user, show)
		}
	}
	for _, slot := range slots {
		for _, user := range slot.Hosts {
			add(user, slot.Show)
		}
	}
	s.mu.Lock()
	s.slots, s.shows = slots, shows
	s.mu.Unlock()
}

func (s *Static) Slots(from, to time.Time) []Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Slot
	for _, slot := range s.slots {
		if slot.Start.Before(to) && slot.End.After(from) {
			out = append(out, slot)
		}
	}
	return out
}

func (s *Static) ShowsFor(username string) []Show {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Show(nil), s.shows[username]...)
}
