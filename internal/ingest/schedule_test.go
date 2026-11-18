package ingest_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/splatboard/icegator/internal/controller"
	"github.com/splatboard/icegator/internal/schedule"
	"github.com/splatboard/icegator/internal/upstreamtest"
)

// The test programme, on an evening that starts at 20:00.
//
//	20:00-21:00  Morning Glory   alice
//	21:00-22:00  Bob's Basement  bob
//	22:00-23:00  Night Shift     bob (a second show of his, straight after)
//	23:00-       nothing
var (
	evening     = time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	morning     = schedule.Show{ID: "show-mg", Name: "Morning Glory"}
	basement    = schedule.Show{ID: "show-bb", Name: "Bob's Basement"}
	nightShift  = schedule.Show{ID: "show-ns", Name: "Night Shift"}
	carolsHour  = schedule.Show{ID: "show-ch", Name: "Carol's Hour"}
	slotMorning = schedule.Slot{ID: "slot-1", Show: morning, Start: at(0), End: at(60), Hosts: []string{"alice"}}
	slotBase    = schedule.Slot{ID: "slot-2", Show: basement, Start: at(60), End: at(120), Hosts: []string{"bob"}}
	slotNight   = schedule.Slot{ID: "slot-3", Show: nightShift, Start: at(120), End: at(180), Hosts: []string{"bob"}}
)

// at is a time the given number of minutes into the evening.
func at(minutes int) time.Time { return evening.Add(time.Duration(minutes) * time.Minute) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// events collects what the controller reports, as short strings.
type events struct {
	mu  sync.Mutex
	log []string
}

func (ev *events) add(e controller.Event) {
	show := "-"
	if e.Turn.Show != nil {
		show = e.Turn.Show.Name
	}
	line := fmt.Sprintf("%s %s %s", e.Kind, e.Turn.DJ.Username, show)
	if e.Kind == "track" {
		line += " " + e.Title
	}
	ev.mu.Lock()
	ev.log = append(ev.log, line)
	ev.mu.Unlock()
}

func (ev *events) all() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.log...)
}

type schedRig struct {
	*rig
	clock  *clock
	events *events
	tapes  *tapes
}

// tapes collects the recordings the controller starts.
type tapes struct {
	mu   sync.Mutex
	list []string // "dj show" per recording started
}

func (tp *tapes) all() []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]string(nil), tp.list...)
}

func newSchedRig(t *testing.T, policy controller.Policy, minutesIn int) *schedRig {
	t.Helper()
	return newSchedRigWith(t, policy, minutesIn, controller.Overrun{Unlimited: true})
}

func newSchedRigWith(t *testing.T, policy controller.Policy, minutesIn int, overrun controller.Overrun) *schedRig {
	t.Helper()
	sched := &schedule.Static{}
	sched.Set([]schedule.Slot{slotMorning, slotBase, slotNight}, map[string][]schedule.Show{"carol": {carolsHour}})
	c := &clock{t: at(minutesIn)}
	ev := &events{}
	tp := &tapes{}
	r := newRig(t, rigOpt{
		schedule: sched, policy: policy, overrun: overrun, now: c.now, onEvent: ev.add,
		record: func(turn controller.Turn) (controller.Recording, error) {
			tp.mu.Lock()
			tp.list = append(tp.list, turn.DJ.Username+" "+turn.Show.Name)
			tp.mu.Unlock()
			return &tape{}, nil
		},
	})
	return &schedRig{rig: r, clock: c, events: ev, tapes: tp}
}

// advance moves the clock to the given minute and lets the controller act.
func (r *schedRig) advance(minutes int) {
	r.clock.mu.Lock()
	r.clock.t = at(minutes)
	r.clock.mu.Unlock()
	r.ctl.Tick()
}

func (r *schedRig) sessionOf(user string) string {
	for _, d := range r.ctl.Status().DJs {
		if d.Username == user {
			return d.SessionID
		}
	}
	r.t.Fatalf("%s is not connected", user)
	return ""
}

// settled gives the controller a moment to do something it should not do.
func settled() { time.Sleep(60 * time.Millisecond) }

func TestScheduledDJWaitsForTheirSlot(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, -10) // ten minutes before the evening starts
	r.put("alice", "AAAA-2222")
	r.waitState("alice", controller.StateQueued)
	settled()
	if r.station.Live() != nil {
		t.Fatal("alice went on air before her slot")
	}
	if st := r.ctl.Status(); st.Current != nil || st.Next == nil || st.Next.Show.Name != "Morning Glory" {
		t.Fatalf("schedule in status: current %+v next %+v", st.Current, st.Next)
	}

	r.advance(0)
	r.waitState("alice", controller.StateOnAir)
	r.waitLive(1)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Morning Glory with Alice"})
	st := r.ctl.Status()
	if st.Show == nil || st.Show.ID != "show-mg" || st.Current.ID != "slot-1" || st.Next.ID != "slot-2" || st.Forced {
		t.Fatalf("status %+v", st)
	}
}

func TestOnlyHostsOfTheShowGoOnAir(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10) // during Morning Glory
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	settled()
	if r.station.Live() != nil {
		t.Fatal("bob went on air in alice's slot")
	}
	r.put("alice", "AAAA-2222")
	r.waitState("alice", controller.StateOnAir)
}

func TestAutoHandoffWaitsForTheNextDJ(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10)
	alice := r.put("alice", "AAAA-2222")
	first := r.waitLive(1)

	// Alice's slot ends, but bob has not turned up. She carries on.
	r.advance(65)
	settled()
	st := r.ctl.Status()
	if r.states()["alice"] != controller.StateOnAir || st.OverrunSince == nil || !st.OverrunSince.Equal(at(60)) {
		t.Fatalf("alice should be overrunning: %+v", st)
	}
	if st.CutAt != nil {
		t.Fatal("a cut is planned although another show is scheduled")
	}

	// Bob connects. The switch happens once his audio is flowing.
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateOnAir)
	r.waitLive(2)
	if !first.Ended() {
		t.Fatal("alice's station connection is still open")
	}
	// Alice is simply off air. She is not held, so a later slot of hers
	// would put her back on.
	r.waitState("alice", controller.StateQueued)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Bob's Basement with Bob"})
	alice.hangUp()

	want := "on_air alice Morning Glory|off_air alice Morning Glory|on_air bob Bob's Basement"
	r.waitFor("events "+want, func() bool { return strings.Join(r.events.all(), "|") == want })
}

func TestAutoHandoffOnTheHourWhenNextDJIsReady(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10)
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	r.advance(59)
	settled()
	if r.states()["bob"] == controller.StateOnAir {
		t.Fatal("bob went on air before his slot")
	}
	r.advance(60)
	r.waitState("bob", controller.StateOnAir)
	r.waitLive(2)
}

func TestManualPolicyWaitsForTheOperator(t *testing.T) {
	r := newSchedRig(t, controller.PolicyManual, 10)
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	if r.ctl.Status().HandoffTo != "" || r.ctl.Handoff() {
		t.Fatal("a handoff to bob was possible before his slot")
	}

	r.advance(61)
	settled()
	if r.states()["alice"] != controller.StateOnAir {
		t.Fatal("the handoff happened without an operator")
	}
	if got := r.ctl.Status().HandoffTo; got != r.sessionOf("bob") {
		t.Fatalf("handoff_to %q, want bob's session", got)
	}
	if !r.ctl.Handoff() {
		t.Fatal("handoff refused")
	}
	r.waitState("bob", controller.StateOnAir)
	r.waitLive(2)
	if r.states()["alice"] != controller.StateQueued {
		t.Fatalf("alice is %s, want off air but not held", r.states()["alice"])
	}
}

func TestUnlimitedOverrunLastsUntilSomeoneWithAClaimArrives(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 130) // during Night Shift, the last show
	r.put("bob", "BBBB-3333")
	r.waitLive(1)

	// Hours after his show ended, with nothing scheduled, he is still on.
	r.advance(600)
	settled()
	st := r.ctl.Status()
	if r.states()["bob"] != controller.StateOnAir || st.CutAt != nil || st.OverrunSince == nil {
		t.Fatalf("bob should be overrunning with no cut planned: %+v", st)
	}
	// A DJ with no claim to the air does not displace him.
	r.put("carol", "CCCC-4444")
	r.waitState("carol", controller.StateQueued)
	settled()
	if r.states()["bob"] != controller.StateOnAir {
		t.Fatal("a DJ with nothing scheduled took the air from an overrunning one")
	}
}

func TestStrictOverrunCutsAtTheEndOfTheSlot(t *testing.T) {
	r := newSchedRigWith(t, controller.PolicyAuto, 10, controller.Overrun{})
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.advance(59)
	settled()
	if r.station.Live() == nil {
		t.Fatal("alice was taken off before her show ended")
	}
	// Her show ends. Bob's is next, but he has not connected, and she is
	// taken off regardless.
	r.advance(60)
	r.waitFor("alice to be taken off air", func() bool { return r.station.Live() == nil })
	if r.states()["alice"] != controller.StateQueued {
		t.Fatalf("alice is %s", r.states()["alice"])
	}
	// The air is his when he does arrive.
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateOnAir)
}

func TestStrictOverrunStillHandsStraightToAReadyDJ(t *testing.T) {
	r := newSchedRigWith(t, controller.PolicyAuto, 10, controller.Overrun{})
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)
	r.advance(60)
	r.waitState("bob", controller.StateOnAir)
	r.waitLive(2)
}

func TestTimedOverrun(t *testing.T) {
	limit := controller.Overrun{Limit: 10 * time.Minute}
	r := newSchedRigWith(t, controller.PolicyAuto, 10, limit)
	r.put("alice", "AAAA-2222")
	r.waitLive(1)

	// Bob is late. Alice has ten minutes past the end of her show.
	r.advance(69)
	settled()
	st := r.ctl.Status()
	if r.station.Live() == nil || st.CutAt == nil || !st.CutAt.Equal(at(70)) {
		t.Fatalf("alice should be on air until 21:10: %+v", st)
	}
	r.advance(70)
	r.waitFor("alice to be taken off air", func() bool { return r.station.Live() == nil })
}

func TestTimedOverrunEndsEarlyWhenTheNextDJIsReady(t *testing.T) {
	r := newSchedRigWith(t, controller.PolicyAuto, 10, controller.Overrun{Limit: 10 * time.Minute})
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.advance(63)
	settled()
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateOnAir)
}

func TestOverrunLimitsDoNotApplyToADJsOwnNextShow(t *testing.T) {
	// Bob hosts two shows back to back. Running from one into the other
	// is not overrunning, even under the strict rule.
	r := newSchedRigWith(t, controller.PolicyAuto, 70, controller.Overrun{})
	r.put("bob", "BBBB-3333")
	r.waitLive(1)
	r.advance(125)
	settled()
	if st := r.ctl.Status(); r.station.Live() == nil || st.Show.ID != "show-ns" || st.CutAt != nil {
		t.Fatalf("bob should be on air with his second show: %+v", st)
	}
}

func TestDropAndReconnectWithinSlot(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10)
	alice := r.put("alice", "AAAA-2222")
	r.waitLive(1)
	alice.hangUp()
	r.waitFor("station idle", func() bool { return r.station.Live() == nil })
	r.put("alice", "AAAA-2222")
	r.waitState("alice", controller.StateOnAir)
	r.waitLive(2)
}

func TestBackToBackShowsShareOneConnection(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 70) // during Bob's Basement
	r.put("bob", "BBBB-3333")
	r.waitLive(1)
	r.title("bob", "BBBB-3333", "Last Song")
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Last Song"})

	// Night Shift is bob's too. He stays on the same station connection,
	// but it is a new turn with its own recording and its own events.
	r.advance(120)
	want := []string{
		"on_air bob Bob's Basement", "track bob Bob's Basement Last Song", "off_air bob Bob's Basement",
		"on_air bob Night Shift", "track bob Night Shift Last Song",
	}
	r.waitFor("events for both turns", func() bool { return strings.Join(r.events.all(), "|") == strings.Join(want, "|") })
	if got := strings.Join(r.tapes.all(), "|"); got != "bob Bob's Basement|bob Night Shift" {
		t.Fatalf("recordings %q", got)
	}
	if n := len(r.station.Sources()); n != 1 {
		t.Fatalf("station saw %d connections, want 1", n)
	}
	if st := r.ctl.Status(); st.Show.ID != "show-ns" || st.OverrunSince != nil {
		t.Fatalf("status %+v", st)
	}
}

func TestOperatorPutsDJOnAirOffSchedule(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10) // alice's slot, and she is absent
	r.put("carol", "CCCC-4444")
	r.waitState("carol", controller.StateQueued)
	carol := r.sessionOf("carol")
	if shows := r.ctl.Status().DJs[0].Shows; len(shows) != 1 || shows[0].ID != "show-ch" {
		t.Fatalf("carol's shows %+v", shows)
	}

	if err := r.ctl.Air(carol, "show-bb"); err == nil {
		t.Fatal("carol was put on air as a show she does not host")
	}

	// With no show named she is on air, but off the record.
	if err := r.ctl.Air(carol, ""); err != nil {
		t.Fatal(err)
	}
	r.waitLive(1)
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Carol"})
	if st := r.ctl.Status(); !st.Forced || st.Show != nil || st.Recording {
		t.Fatalf("status %+v", st)
	}
	if len(r.tapes.all()) != 0 {
		t.Fatal("a turn with no show was recorded")
	}

	// Naming one of her shows starts a turn that is recorded and reported.
	if err := r.ctl.Air(carol, "show-ch"); err != nil {
		t.Fatal(err)
	}
	r.waitTitle(upstreamtest.Title{Username: "icegator", Song: "Carol's Hour with Carol"})
	r.waitFor("recording of carol's hour", func() bool { return strings.Join(r.tapes.all(), "|") == "carol Carol's Hour" })

	// She is not subject to the overrun cut.
	r.advance(59)
	if r.ctl.Status().CutAt != nil {
		t.Fatal("a cut is planned for a DJ an operator put on air")
	}

	// The scheduled DJ turning up ends it.
	r.put("alice", "AAAA-2222")
	r.waitState("alice", controller.StateOnAir)
	r.waitFor("carol off air", func() bool { return r.states()["carol"] == controller.StateQueued })
	// Events are reported from the on-air path as it goes, so give alice's
	// a moment to arrive.
	want := "on_air carol -|off_air carol -|on_air carol Carol's Hour|off_air carol Carol's Hour|on_air alice Morning Glory"
	r.waitFor("events "+want, func() bool { return strings.Contains(strings.Join(r.events.all(), "|"), want) })
}

func TestPulledDJStaysOffUntilPutBack(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10)
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	alice := r.sessionOf("alice")
	if err := r.ctl.Pull(alice); err != nil {
		t.Fatal(err)
	}
	r.waitFor("station idle", func() bool { return r.station.Live() == nil })
	r.advance(20)
	settled()
	if r.states()["alice"] != controller.StateHeld || r.station.Live() != nil {
		t.Fatal("a pulled DJ went back on air by herself")
	}
	if err := r.ctl.Air(alice, ""); err != nil {
		t.Fatal(err)
	}
	r.waitLive(2)
}

func TestOperatorChoiceOverAReadyDJStandsForTheSlot(t *testing.T) {
	r := newSchedRig(t, controller.PolicyAuto, 10)
	r.put("alice", "AAAA-2222")
	r.waitLive(1)
	r.put("carol", "CCCC-4444")
	r.waitState("carol", controller.StateQueued)
	r.put("bob", "BBBB-3333")
	r.waitState("bob", controller.StateQueued)

	// Alice is on air and on schedule. The operator puts carol on instead.
	if err := r.ctl.Air(r.sessionOf("carol"), ""); err != nil {
		t.Fatal(err)
	}
	r.waitLive(2)
	r.advance(20)
	settled()
	if s := r.states(); s["carol"] != controller.StateOnAir || s["alice"] != controller.StateQueued {
		t.Fatalf("the schedule took the air back from the operator's choice: %+v", s)
	}
	// Handing back to alice is one press away.
	if got := r.ctl.Status().HandoffTo; got != r.sessionOf("alice") {
		t.Fatalf("handoff_to %q, want alice", got)
	}

	// The choice lasts for that slot only.
	r.advance(60)
	r.waitState("bob", controller.StateOnAir)
}
