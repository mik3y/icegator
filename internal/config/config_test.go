package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestMinimalForward(t *testing.T) {
	c, err := Parse([]byte("# secrets look like ${THIS}\nupstream:\n  url: http://station:8005/\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth != "forward" || c.Listen.Ingest != ":8000" || c.Handoff.Policy != "manual" ||
		c.Handoff.StationConnection != "reopen" {
		t.Fatalf("defaults: %+v", c)
	}
	if got := c.FallbackTitle("Night Owl", ""); got != "Night Owl" {
		t.Fatal(got)
	}
	c.Metadata.Fallback = "{show} with {dj}"
	if got := c.FallbackTitle("Night Owl", "Late Shift"); got != "Late Shift with Night Owl" {
		t.Fatal(got)
	}
	if got := c.FallbackTitle("Night Owl", ""); got != "Night Owl" {
		t.Fatal(got)
	}
}

func TestLocalWithEnv(t *testing.T) {
	c, err := Parse([]byte(`
auth: local
upstream: {url: "http://station:8005/", username: icegator, password: "${PW}"}
djs:
  - {slug: nightowl, name: Night Owl, key: A573-1BZX}
`), env(map[string]string{"PW": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.Password != "s3cret" || len(c.DJs) != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestStreamlover(t *testing.T) {
	c, err := Parse([]byte(`
upstream: {url: "http://station:8005/", username: icegator, password: pw}
streamlover: {url: "https://sl.example", station: ktest, api_key: "${KEY}"}
`), env(map[string]string{"KEY": "slk_secret_x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode() != "streamlover" || c.Auth != "local" || c.Handoff.Policy != "auto" ||
		c.Streamlover.Visibility != "private" || c.Streamlover.UploadRecordings || c.Streamlover.APIKey != "slk_secret_x" {
		t.Fatalf("%+v", c)
	}
	if got := c.FallbackTitle("Night Owl", "Late Shift"); got != "Late Shift with Night Owl" {
		t.Fatal(got)
	}
}

func TestOverrun(t *testing.T) {
	for setting, want := range map[string]struct {
		unlimited bool
		limit     time.Duration
	}{
		"":       {true, 0},
		"none":   {true, 0},
		"strict": {false, 0},
		"60s":    {false, time.Minute},
		"10m":    {false, 10 * time.Minute},
	} {
		unlimited, limit, err := Handoff{Overrun: setting}.OverrunLimit()
		if err != nil || unlimited != want.unlimited || limit != want.limit {
			t.Errorf("%q: unlimited=%v limit=%v err=%v", setting, unlimited, limit, err)
		}
	}
}

func TestPolicyDefaultFollowsTheMode(t *testing.T) {
	queue, err := Parse([]byte("upstream: {url: x}\n"), env(nil))
	if err != nil || queue.Handoff.Policy != "manual" {
		t.Fatalf("a queue: %q %v", queue.Handoff.Policy, err)
	}
	const followed = "upstream: {url: x, username: u, password: p}\nstreamlover: {url: \"https://sl\", station: s, api_key: k}\n"
	sched, err := Parse([]byte(followed), env(nil))
	if err != nil || sched.Handoff.Policy != "auto" {
		t.Fatalf("a schedule: %q %v", sched.Handoff.Policy, err)
	}
	// Either can be set against its default.
	sched, err = Parse([]byte(followed+"handoff: {policy: manual}\n"), env(nil))
	if err != nil || sched.Handoff.Policy != "manual" {
		t.Fatalf("a schedule set to manual: %q %v", sched.Handoff.Policy, err)
	}
	// The old name is not quietly accepted.
	if _, err := Parse([]byte("upstream: {url: x}\nhandoff: {takeover: manual}\n"), env(nil)); err == nil {
		t.Fatal("handoff.takeover was accepted")
	}
}

func TestStreamloverRecordings(t *testing.T) {
	const base = "upstream: {url: x, username: u, password: p}\nstreamlover:\n  url: \"https://sl\"\n  station: s\n  api_key: k\n"
	c, err := Parse([]byte(base+"  upload_recordings: true\n  visibility: public\n"), env(nil))
	if err != nil || !c.Streamlover.UploadRecordings || c.Streamlover.Visibility != "public" {
		t.Fatalf("%+v %v", c.Streamlover, err)
	}
	if _, err := Parse([]byte(base+"  visibility: secret\n"), env(nil)); err == nil ||
		!strings.Contains(err.Error(), "streamlover.visibility") {
		t.Fatalf("bad visibility: %v", err)
	}
	// Recording to a bucket of one's own is no longer an option, and a
	// config that still asks for it is told so, not quietly ignored.
	if _, err := Parse([]byte("upstream: {url: x}\nrecording: {url: \"https://h/b\"}\n"), env(nil)); err == nil {
		t.Fatal("a recording block was accepted")
	}
}

func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no upstream":      {"auth: forward\n", "upstream.url is required"},
		"typo":             {"upstream: {url: x}\nhandof: {}\n", "handof"},
		"unset env":        {"upstream: {url: \"${NOPE}\"}\n", "NOPE"},
		"djs but forward":  {"upstream: {url: x}\ndjs: [{slug: a, key: AAAA-AAAA}]\n", "set auth: local"},
		"local, no djs":    {"auth: local\nupstream: {url: x, username: u, password: p}\n", "no djs"},
		"local, no acct":   {"auth: local\nupstream: {url: x}\ndjs: [{slug: a, key: AAAA-AAAA}]\n", "upstream.username"},
		"sl, no key":       {"upstream: {url: x, username: u, password: p}\nstreamlover: {url: \"https://sl\", station: s}\n", "streamlover.api_key"},
		"sl, forward":      {"auth: forward\nupstream: {url: x}\nstreamlover: {url: \"https://sl\", station: s, api_key: k}\n", "stream keys"},
		"sl, djs":          {"upstream: {url: x, username: u, password: p}\nstreamlover: {url: \"https://sl\", station: s, api_key: k}\ndjs: [{slug: a, key: AAAA-AAAA}]\n", "remove the djs"},
		"sl, no account":   {"upstream: {url: x}\nstreamlover: {url: \"https://sl\", station: s, api_key: k}\n", "upstream.username"},
		"bad overrun":      {"upstream: {url: x}\nhandoff: {overrun: forever}\n", "handoff.overrun"},
		"negative overrun": {"upstream: {url: x}\nhandoff: {overrun: -5m}\n", "handoff.overrun"},
		"bad policy":       {"upstream: {url: x}\nhandoff: {policy: maybe}\n", "handoff.policy"},
		"bad connection":   {"upstream: {url: x}\nhandoff: {station_connection: maybe}\n", "station_connection"},
		"keep, forward":    {"upstream: {url: x}\nhandoff: {station_connection: keep}\n", "needs auth: local"},
	} {
		_, err := Parse([]byte(tc.yaml), env(nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want mention of %q", name, err, tc.want)
		}
	}
}
