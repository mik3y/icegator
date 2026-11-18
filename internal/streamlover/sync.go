package streamlover

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/splatboard/icegator/internal/auth"
	"github.com/splatboard/icegator/internal/schedule"
)

// How much schedule each sync asks for. The controller looks a day back to
// work out when an overrunning show should be cut.
const (
	lookBack  = 24 * time.Hour
	lookAhead = 48 * time.Hour
)

// Station is Icegator's copy of what StreamLover knows about the station.
// It keeps the DJ list and the schedule up to date, and goes on serving the
// last copy it got when StreamLover cannot be reached.
type Station struct {
	client *Client
	log    *slog.Logger
	// path is where the last good copy is kept, so that a restart during
	// an outage still has DJs and a schedule. Empty keeps none.
	path string

	// DJs checks stream keys against the current list.
	DJs *auth.Local
	// Schedule is the current schedule.
	Schedule *schedule.Static

	mu       sync.Mutex
	syncedAt time.Time
	lastErr  error
}

// snapshot is what one sync learned, and what is kept on disk. The digests
// in it are no use without the API key, which is not stored with them.
type snapshot struct {
	SyncedAt time.Time                  `json:"synced_at"`
	DJs      []auth.DJ                  `json:"djs"`
	Slots    []schedule.Slot            `json:"slots"`
	Hosting  map[string][]schedule.Show `json:"hosting"`
}

// NewStation makes a station that has not synced yet. If statePath holds a
// copy from an earlier run it is loaded straight away.
func NewStation(client *Client, statePath string, log *slog.Logger) *Station {
	if log == nil {
		log = slog.Default()
	}
	s := &Station{client: client, log: log, path: statePath, Schedule: &schedule.Static{}}
	s.DJs, _ = auth.NewLocal(nil)
	s.DJs.Digest = client.StreamKeyDigest
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			var snap snapshot
			if json.Unmarshal(b, &snap) == nil && s.apply(snap) == nil {
				s.syncedAt = snap.SyncedAt
				log.Info("loaded the last copy of the station", "synced_at", snap.SyncedAt, "djs", len(snap.DJs))
			}
		}
	}
	return s
}

func (s *Station) apply(snap snapshot) error {
	if err := s.DJs.Replace(snap.DJs); err != nil {
		return err
	}
	s.Schedule.Set(snap.Slots, snap.Hosting)
	return nil
}

// Sync fetches the DJs, shows and schedule and puts them into effect.
func (s *Station) Sync(ctx context.Context) error {
	err := s.sync(ctx)
	s.mu.Lock()
	s.lastErr = err
	if err == nil {
		s.syncedAt = time.Now()
	}
	s.mu.Unlock()
	return err
}

func (s *Station) sync(ctx context.Context) error {
	hosts, err := s.client.Hosts(ctx)
	if err != nil {
		return fmt.Errorf("reading djs: %w", err)
	}
	shows, err := s.client.Shows(ctx)
	if err != nil {
		return fmt.Errorf("reading shows: %w", err)
	}
	now := time.Now()
	occurrences, err := s.client.Occurrences(ctx, now.Add(-lookBack), now.Add(lookAhead))
	if err != nil {
		return fmt.Errorf("reading the schedule: %w", err)
	}

	snap := snapshot{SyncedAt: now, Hosting: map[string][]schedule.Show{}}
	for _, h := range hosts {
		if h.StreamKeyHMAC != "" && h.Slug != "" {
			snap.DJs = append(snap.DJs, auth.DJ{Slug: h.Slug, Name: h.Name, Key: h.StreamKeyHMAC})
		}
	}
	hostsOf := map[string][]Host{} // show id -> its DJs, by slug
	for _, show := range shows {
		for _, h := range show.Hosts {
			hostsOf[show.ID] = append(hostsOf[show.ID], h)
			snap.Hosting[h.Slug] = append(snap.Hosting[h.Slug], schedule.Show{ID: show.ID, Name: show.Name})
		}
		sort.Slice(hostsOf[show.ID], func(i, j int) bool { return hostsOf[show.ID][i].Slug < hostsOf[show.ID][j].Slug })
	}
	for _, o := range occurrences {
		var slugs, names []string
		for _, h := range hostsOf[o.Show.ID] {
			slugs, names = append(slugs, h.Slug), append(names, h.Name)
		}
		snap.Slots = append(snap.Slots, schedule.Slot{
			// The schedule rule plus the occurrence's original start is
			// what StreamLover itself identifies an occurrence by, and it
			// survives the occurrence being moved.
			ID:    o.ScheduleID + "@" + o.OriginalStart.UTC().Format("20060102T150405Z"),
			Show:  schedule.Show{ID: o.Show.ID, Name: o.Show.Name},
			Start: o.Start, End: o.End,
			Hosts: slugs, HostNames: names,
		})
	}
	if err := s.apply(snap); err != nil {
		return err
	}
	s.save(snap)
	return nil
}

func (s *Station) save(snap snapshot) {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

// Run syncs now and then every interval until ctx is cancelled. A failed
// sync is logged and the last copy stays in force.
func (s *Station) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	failing := false
	for {
		if err := s.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("could not sync with streamlover; carrying on with the last copy", "err", err)
			failing = true
		} else if failing {
			s.log.Info("syncing with streamlover again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Status reports when the station last synced and what went wrong last
// time, if anything.
func (s *Station) Status() (syncedAt time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncedAt, s.lastErr
}
