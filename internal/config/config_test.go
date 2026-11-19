package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestStatic(t *testing.T) {
	c, err := Parse([]byte(`
# secrets look like ${THIS}
upstream: {url: "http://station:8005/", username: icegator, password: "${PW}"}
provider:
  static:
    djs:
      - {slug: nightowl, name: Night Owl, key: A573-1BZX}
`), env(map[string]string{"PW": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.Name() != "static" || c.Provider.Scheduled() || len(c.Provider.Static.DJs) != 1 ||
		c.Upstream.Password != "s3cret" || c.Listen.Ingest != ":8000" || c.Handoff.Policy != "manual" ||
		c.Handoff.StationConnection != "reopen" || c.Handoff.Overrun != "" {
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

func TestStreamlover(t *testing.T) {
	c, err := Parse([]byte(`
upstream: {url: "http://station:8005/", username: icegator, password: pw}
provider:
  streamlover: {url: "https://sl.example", station: ktest, api_key: "${KEY}"}
`), env(map[string]string{"KEY": "slk_secret_x"}))
	if err != nil {
		t.Fatal(err)
	}
	sl := c.Provider.Streamlover
	if c.Provider.Name() != "streamlover" || !c.Provider.Scheduled() || c.Handoff.Policy != "auto" ||
		c.Handoff.Overrun != "none" || sl.SyncInterval != time.Minute ||
		sl.Visibility != "private" || sl.UploadRecordings || sl.APIKey != "slk_secret_x" {
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

func TestPolicyDefaultFollowsTheProvider(t *testing.T) {
	const account = "upstream: {url: x, username: u, password: p}\n"
	queue, err := Parse([]byte(account+"provider: {static: {djs: [{slug: a, key: AAAA-AAAA}]}}\n"), env(nil))
	if err != nil || queue.Handoff.Policy != "manual" {
		t.Fatalf("a queue: %q %v", queue.Handoff.Policy, err)
	}
	const followed = account + "provider: {streamlover: {url: \"https://sl\", station: s, api_key: k}}\n"
	sched, err := Parse([]byte(followed), env(nil))
	if err != nil || sched.Handoff.Policy != "auto" {
		t.Fatalf("a schedule: %q %v", sched.Handoff.Policy, err)
	}
	// Either can be set against its default.
	sched, err = Parse([]byte(followed+"handoff: {policy: manual}\n"), env(nil))
	if err != nil || sched.Handoff.Policy != "manual" {
		t.Fatalf("a schedule set to manual: %q %v", sched.Handoff.Policy, err)
	}
}

func TestStreamloverRecordings(t *testing.T) {
	const base = "upstream: {url: x, username: u, password: p}\nprovider:\n  streamlover:\n    url: \"https://sl\"\n    station: s\n    api_key: k\n"
	c, err := Parse([]byte(base+"    upload_recordings: true\n    visibility: public\n"), env(nil))
	if err != nil || !c.Provider.Streamlover.UploadRecordings || c.Provider.Streamlover.Visibility != "public" {
		t.Fatalf("%+v %v", c.Provider.Streamlover, err)
	}
	if _, err := Parse([]byte(base+"    visibility: secret\n"), env(nil)); err == nil ||
		!strings.Contains(err.Error(), "provider.streamlover.visibility") {
		t.Fatalf("bad visibility: %v", err)
	}
}

func TestErrors(t *testing.T) {
	const (
		account = "upstream: {url: x, username: u, password: p}\n"
		static  = "provider: {static: {djs: [{slug: a, key: AAAA-AAAA}]}}\n"
		sl      = "provider: {streamlover: {url: \"https://sl\", station: s, api_key: k}}\n"
	)
	for name, tc := range map[string]struct{ yaml, want string }{
		"no upstream":      {static, "upstream.url is required"},
		"no account":       {"upstream: {url: x}\n" + static, "upstream.username"},
		"typo":             {account + static + "handof: {}\n", "handof"},
		"unset env":        {"upstream: {url: \"${NOPE}\"}\n", "NOPE"},
		"no provider":      {account, "provider is required"},
		"empty provider":   {account + "provider: {}\n", "provider is required"},
		"two providers":    {account + "provider: {static: {djs: [{slug: a, key: AAAA-AAAA}]}, streamlover: {url: \"https://sl\", station: s, api_key: k}}\n", "exactly one"},
		"unknown provider": {account + "provider: {azuracast: {}}\n", "azuracast"},
		"static, no djs":   {account + "provider: {static: {}}\n", "lists no DJs"},
		"sl, no key":       {account + "provider: {streamlover: {url: \"https://sl\", station: s}}\n", "api_key"},
		"sl, bad url":      {account + "provider: {streamlover: {url: sl, station: s, api_key: k}}\n", "https://"},
		"bad overrun":      {account + sl + "handoff: {overrun: forever}\n", "handoff.overrun"},
		"negative overrun": {account + sl + "handoff: {overrun: -5m}\n", "handoff.overrun"},
		"overrun, static":  {account + static + "handoff: {overrun: 10m}\n", "no schedule"},
		"bad policy":       {account + static + "handoff: {policy: maybe}\n", "handoff.policy"},
		"bad connection":   {account + static + "handoff: {station_connection: maybe}\n", "station_connection"},
		// Settings from before there were providers are not quietly accepted.
		"old auth":        {account + static + "auth: local\n", "auth"},
		"old djs":         {account + static + "djs: [{slug: a, key: AAAA-AAAA}]\n", "djs"},
		"old streamlover": {account + static + "streamlover: {url: \"https://sl\"}\n", "streamlover"},
	} {
		_, err := Parse([]byte(tc.yaml), env(nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want mention of %q", name, err, tc.want)
		}
	}
}
