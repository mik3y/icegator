package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// unset clears variables for the length of a test.
func unset(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "") // registers the restore
		os.Unsetenv(name)
	}
}

func TestLoadEnvBesideTheConfig(t *testing.T) {
	unset(t, "IG_TEST_URL", "IG_TEST_PW", "IG_TEST_SHARED")
	dir := t.TempDir()
	cfg := write(t, dir, "icegator.yaml", "upstream: {url: \"${IG_TEST_URL}\", username: u, password: \"${IG_TEST_PW}\"}\nprovider: {static: {djs: [{slug: a, key: AAAA-AAAA}]}}\n")
	write(t, dir, ".env", "IG_TEST_URL=http://from-env:8005/\nIG_TEST_SHARED=from-env\n")
	write(t, dir, ".env.local", "IG_TEST_PW='s3cret # not a comment'\nIG_TEST_SHARED=from-local\n")

	read, err := LoadEnv(cfg, "")
	if err != nil || len(read) != 2 {
		t.Fatalf("read %v, err %v", read, err)
	}
	// The machine's own file wins over the shared one.
	if got := os.Getenv("IG_TEST_SHARED"); got != "from-local" {
		t.Fatalf("IG_TEST_SHARED=%q", got)
	}
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.URL != "http://from-env:8005/" || c.Upstream.Password != "s3cret # not a comment" {
		t.Fatalf("%+v", c.Upstream)
	}
}

func TestTheEnvironmentWinsOverEnvFiles(t *testing.T) {
	t.Setenv("IG_TEST_URL", "http://from-the-shell/")
	dir := t.TempDir()
	write(t, dir, ".env", "IG_TEST_URL=http://from-env/\n")
	if _, err := LoadEnv(filepath.Join(dir, "icegator.yaml"), ""); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("IG_TEST_URL"); got != "http://from-the-shell/" {
		t.Fatalf("IG_TEST_URL=%q", got)
	}
}

func TestNoEnvFilesIsFine(t *testing.T) {
	read, err := LoadEnv(filepath.Join(t.TempDir(), "icegator.yaml"), "")
	if err != nil || len(read) != 0 {
		t.Fatalf("read %v, err %v", read, err)
	}
}

func TestAnExplicitEnvFile(t *testing.T) {
	unset(t, "IG_TEST_URL")
	dir := t.TempDir()
	// Files beside the config are not read when one is named.
	write(t, dir, ".env", "IG_TEST_URL=http://beside/\n")
	named := write(t, dir, "prod.env", "IG_TEST_URL=http://named/\n")
	if _, err := LoadEnv(filepath.Join(dir, "icegator.yaml"), named); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("IG_TEST_URL"); got != "http://named/" {
		t.Fatalf("IG_TEST_URL=%q", got)
	}
	if _, err := LoadEnv(filepath.Join(dir, "icegator.yaml"), filepath.Join(dir, "missing.env")); err == nil {
		t.Fatal("a named file that does not exist was accepted")
	}
}

func TestAMalformedEnvFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "this is not an assignment\n")
	if _, err := LoadEnv(filepath.Join(dir, "icegator.yaml"), ""); err == nil {
		t.Fatal("accepted")
	}
}
