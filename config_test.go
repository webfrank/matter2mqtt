package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	const file = `
# a comment
MATTER_WS_URL=ws://192.168.1.4:5580/ws
  export HTTP_ENABLED=true

MQTT_TOPIC_PREFIX = matter
QUOTED="matter"
SINGLE='keep # this'
HASH_IN_VALUE=p@ss#word
ESCAPED="line\tbreak"
EMPTY=
`
	for _, k := range []string{
		"MATTER_WS_URL", "HTTP_ENABLED", "MQTT_TOPIC_PREFIX",
		"QUOTED", "SINGLE", "HASH_IN_VALUE", "ESCAPED", "EMPTY",
	} {
		t.Setenv(k, "") // registers the key for cleanup
		os.Unsetenv(k)
	}

	n, err := parseDotEnv(strings.NewReader(file))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != 8 {
		t.Fatalf("set %d vars, want 8", n)
	}

	want := map[string]string{
		"MATTER_WS_URL":     "ws://192.168.1.4:5580/ws",
		"HTTP_ENABLED":      "true",
		"MQTT_TOPIC_PREFIX": "matter", // spaces around = are trimmed
		"QUOTED":            "matter",
		"SINGLE":            "keep # this", // no inline comments, quotes stripped
		"HASH_IN_VALUE":     "p@ss#word",   // a # mid-value is part of it
		"ESCAPED":           "line\tbreak",
		"EMPTY":             "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestDotEnvDoesNotOverrideTheRealEnvironment(t *testing.T) {
	t.Setenv("MQTT_BROKER", "tcp://real:1883")

	if _, err := parseDotEnv(strings.NewReader("MQTT_BROKER=tcp://file:1883\n")); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := os.Getenv("MQTT_BROKER"); got != "tcp://real:1883" {
		t.Fatalf("MQTT_BROKER = %q, the file overrode the environment", got)
	}
}

func TestParseDotEnvRejectsMalformedLines(t *testing.T) {
	for _, in := range []string{"not a pair\n", "=novalue\n", "  = x\n"} {
		if _, err := parseDotEnv(strings.NewReader(in)); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // away from the repo's own .env, so the default case is clean
	path := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(path, []byte("MATTER_CALL_TIMEOUT=90s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ENV_FILE", path)
	t.Setenv("MATTER_CALL_TIMEOUT", "")
	os.Unsetenv("MATTER_CALL_TIMEOUT")

	got, n, err := LoadDotEnv()
	if err != nil || n != 1 || got != path {
		t.Fatalf("LoadDotEnv() = %q, %d, %v", got, n, err)
	}
	if v := os.Getenv("MATTER_CALL_TIMEOUT"); v != "90s" {
		t.Fatalf("MATTER_CALL_TIMEOUT = %q", v)
	}

	// An explicitly requested file that is absent is an error, unlike the
	// default .env, whose absence is normal.
	t.Setenv("ENV_FILE", filepath.Join(dir, "missing.env"))
	if _, _, err := LoadDotEnv(); err == nil {
		t.Fatal("a missing ENV_FILE should fail loudly")
	}

	t.Setenv("ENV_FILE", "")
	os.Unsetenv("ENV_FILE")
	if _, n, err := LoadDotEnv(); err != nil || n != 0 {
		t.Fatalf("absent default .env should be a silent no-op: %d, %v", n, err)
	}
}
