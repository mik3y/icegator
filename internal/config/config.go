// Package config loads and validates Icegator's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Station  Station  `yaml:"station"`
	Listen   Listen   `yaml:"listen"`
	Upstream Upstream `yaml:"upstream"`
	// Auth is "forward" or "local". Under "forward", the default, DJs' own
	// station logins are passed through. Under "local", Icegator checks
	// the keys under `djs`.
	Auth     string   `yaml:"auth"`
	Handoff  Handoff  `yaml:"handoff"`
	Metadata Metadata `yaml:"metadata"`
	DJs      []DJ     `yaml:"djs"`
	// Operators may sign in to the dashboard. With none listed, the
	// dashboard is not served.
	Operators []Operator `yaml:"operators"`
	Levels    Levels     `yaml:"levels"`
	// StateDir holds what must survive a restart. Default "./state".
	StateDir string `yaml:"state_dir"`
}

type Station struct {
	Name string `yaml:"name"`
}

type Listen struct {
	Ingest    string `yaml:"ingest"`    // DJs connect here; default ":8000"
	Dashboard string `yaml:"dashboard"` // operators; default ":8080"
}

type Upstream struct {
	// URL is the station's source address including the mount, e.g.
	// http://azuracast.example.com:8005/
	URL string `yaml:"url"`
	// Username and Password are the account Icegator streams as. Needed
	// with `auth: local` and unused with `auth: forward`.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Handoff struct {
	// Policy says what happens when a DJ connects while another is on
	// air. "manual" (the default) keeps the DJ on air there until they
	// disconnect or an operator hands off. "auto" puts the newcomer on as
	// soon as their audio is flowing.
	Policy string `yaml:"policy"`
	// StationConnection says what happens to the connection to the
	// station at a handoff. "reopen" (the default) closes it and opens a
	// new one for the next DJ, so the station plays its own programming
	// for a moment in between. "keep" hands the same connection to the
	// next DJ, so the station hears one continuous source. Nothing is
	// re-encoded either way. "keep" needs `auth: local`.
	StationConnection string `yaml:"station_connection"`
	// ReadyAfter is how much audio a DJ must send before they can go on
	// air. Default 3s.
	ReadyAfter time.Duration `yaml:"ready_after"`
}

type Metadata struct {
	// Fallback is the title shown when the DJ has sent none. "{dj}" is
	// replaced by the DJ's name and "{show}" by the show's. Default "{dj}".
	Fallback string `yaml:"fallback"`
}

type DJ struct {
	Slug string `yaml:"slug"`
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type Operator struct {
	Username string `yaml:"username"`
	// PasswordHash is a bcrypt hash; make one with `icegator -hash-password`.
	PasswordHash string `yaml:"password_hash"`
}

// Levels are the loudness thresholds (dBFS, RMS over a few seconds) behind
// the dashboard's low / good / high verdict. Zero means the default.
type Levels struct {
	SilentDBFS float64 `yaml:"silent_dbfs"` // default -60
	LowDBFS    float64 `yaml:"low_dbfs"`    // default -24
	HighDBFS   float64 `yaml:"high_dbfs"`   // default -5
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the file at path. "${NAME}" anywhere in it is replaced by that
// environment variable, so secrets can stay out of the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, os.LookupEnv)
}

func Parse(raw []byte, lookup func(string) (string, bool)) (*Config, error) {
	var missing []string
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		// Comments are left alone, so that one can mention ${NAME}.
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			continue
		}
		lines[i] = envRef.ReplaceAllFunc(line, func(m []byte) []byte {
			name := string(envRef.FindSubmatch(m)[1])
			v, ok := lookup(name)
			if !ok {
				missing = append(missing, name)
			}
			return []byte(v)
		})
	}
	raw = bytes.Join(lines, []byte("\n"))
	if len(missing) > 0 {
		return nil, fmt.Errorf("config refers to unset environment variables: %s", strings.Join(missing, ", "))
	}

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo in a key should not be silently ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	return &c, c.validate()
}

func (c *Config) defaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.Listen.Ingest, ":8000")
	def(&c.Listen.Dashboard, ":8080")
	def(&c.Auth, "forward")
	def(&c.Handoff.Policy, "manual")
	def(&c.Handoff.StationConnection, "reopen")
	def(&c.Metadata.Fallback, "{dj}")
	def(&c.StateDir, "state")
	if c.Handoff.ReadyAfter == 0 {
		c.Handoff.ReadyAfter = 3 * time.Second
	}
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Upstream.URL == "" {
		bad("upstream.url is required")
	}
	switch c.Auth {
	case "forward":
		if len(c.DJs) > 0 {
			bad("djs are listed but auth is \"forward\"; set auth: local to use stream keys")
		}
		if c.Upstream.Username != "" || c.Upstream.Password != "" {
			bad("upstream.username/password are set but auth is \"forward\", which uses each DJ's own login; remove them or set auth: local")
		}
	case "local":
		if len(c.DJs) == 0 {
			bad("auth is \"local\" but no djs are listed")
		}
		if c.Upstream.Username == "" || c.Upstream.Password == "" {
			bad("auth is \"local\", so upstream.username and upstream.password are required")
		}
	default:
		bad("auth must be \"forward\" or \"local\", not %q", c.Auth)
	}
	seen := map[string]bool{}
	for _, op := range c.Operators {
		switch {
		case op.Username == "":
			bad("an operator has no username")
		case seen[op.Username]:
			bad("operator %q is listed twice", op.Username)
		case !strings.HasPrefix(op.PasswordHash, "$2"):
			bad("operator %q: password_hash must be a bcrypt hash (make one with: icegator -hash-password)", op.Username)
		}
		seen[op.Username] = true
	}
	if l := c.Levels; l.SilentDBFS > 0 || l.LowDBFS > 0 || l.HighDBFS > 0 {
		bad("levels thresholds are dBFS values and must be negative")
	}
	switch c.Handoff.StationConnection {
	case "reopen":
	case "keep":
		if c.Auth == "forward" {
			bad("handoff.station_connection is \"keep\", which needs auth: local; with auth: forward each DJ connects to the station as themselves")
		}
	default:
		bad("handoff.station_connection must be \"reopen\" or \"keep\", not %q", c.Handoff.StationConnection)
	}
	if p := c.Handoff.Policy; p != "auto" && p != "manual" {
		bad("handoff.policy must be \"auto\" or \"manual\", not %q", p)
	}
	return errors.Join(errs...)
}

// FallbackTitle fills in the metadata.fallback template. A template that
// mentions the show is no use when there is none, so the DJ's name is used
// alone in that case.
func (c *Config) FallbackTitle(djName, showName string) string {
	if showName == "" && strings.Contains(c.Metadata.Fallback, "{show}") {
		return djName
	}
	return strings.NewReplacer("{dj}", djName, "{show}", showName).Replace(c.Metadata.Fallback)
}
