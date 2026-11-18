package config

import (
	"strings"
	"testing"
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

func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no upstream":     {"auth: forward\n", "upstream.url is required"},
		"typo":            {"upstream: {url: x}\nhandof: {}\n", "handof"},
		"unset env":       {"upstream: {url: \"${NOPE}\"}\n", "NOPE"},
		"djs but forward": {"upstream: {url: x}\ndjs: [{slug: a, key: AAAA-AAAA}]\n", "set auth: local"},
		"local, no djs":   {"auth: local\nupstream: {url: x, username: u, password: p}\n", "no djs"},
		"local, no acct":  {"auth: local\nupstream: {url: x}\ndjs: [{slug: a, key: AAAA-AAAA}]\n", "upstream.username"},
		"bad policy":      {"upstream: {url: x}\nhandoff: {policy: maybe}\n", "handoff.policy"},
		"bad connection":  {"upstream: {url: x}\nhandoff: {station_connection: maybe}\n", "station_connection"},
		"keep, forward":   {"upstream: {url: x}\nhandoff: {station_connection: keep}\n", "needs auth: local"},
	} {
		_, err := Parse([]byte(tc.yaml), env(nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want mention of %q", name, err, tc.want)
		}
	}
}
