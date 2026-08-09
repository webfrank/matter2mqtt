package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable for the bridge. Everything comes from the
// environment so the service stays container-friendly and stateless.
type Config struct {
	// Matter server (python-matter-server) websocket endpoint.
	MatterWSURL string
	// How long to wait for a single matter-server command to complete.
	MatterCallTimeout time.Duration

	// MQTT broker, e.g. tcp://mqtt.example.com:1883 or ssl://host:8883
	MQTTBroker   string
	MQTTUsername string
	MQTTPassword string
	MQTTClientID string
	MQTTQoS      byte
	// Retain attribute state topics so late subscribers get current values.
	RetainState bool

	// Root of the topic tree, no trailing slash.
	TopicPrefix string

	// Publish a human-readable mirror of every attribute topic under
	// <prefix>/named/... in addition to the canonical numeric tree.
	PublishNames bool
	// Optional path to a generated model.json, overriding the embedded seed.
	ModelPath string

	// Embedded web console.
	HTTPEnabled  bool
	HTTPAddr     string
	HTTPUsername string
	HTTPPassword string

	LogLevel string
}

// defaultEnvFile is consulted when ENV_FILE is unset. Missing is not an error:
// a container gets its settings from the compose environment block, and only a
// local run needs the file.
const defaultEnvFile = ".env"

// LoadDotEnv reads KEY=VALUE pairs from a .env file into the process
// environment, so running the binary straight from a checkout picks up the same
// settings a container gets from compose. It returns the file it read and how
// many variables it set.
//
// The real environment always wins: a variable already exported is never
// overwritten, so a container or a one-off `MQTT_BROKER=... ./matter2mqtt`
// still overrides the file.
//
// ENV_FILE selects a different path, and unlike the default it must exist -
// asking for a file that is not there is a configuration error, not a silent
// no-op. It is read from the real environment only, for obvious reasons.
func LoadDotEnv() (path string, set int, err error) {
	path = os.Getenv("ENV_FILE")
	explicit := path != ""
	if !explicit {
		path = defaultEnvFile
	}

	f, err := os.Open(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return "", 0, nil
		}
		return path, 0, err
	}
	defer f.Close()

	set, err = parseDotEnv(f)
	if err != nil {
		return path, set, fmt.Errorf("%s: %w", path, err)
	}
	return path, set, nil
}

// parseDotEnv follows the same rules as compose's env_file, so one file can
// feed both: blank lines and lines whose first non-space character is `#` are
// skipped, an `export ` prefix is tolerated, and everything after the first `=`
// is the value - there are no inline comments, which keeps a `#` inside a
// password intact. Surrounding quotes are stripped, and escapes inside double
// quotes are interpreted.
func parseDotEnv(r io.Reader) (set int, err error) {
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")

		key, val, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return set, fmt.Errorf("line %d: %q is not KEY=VALUE", line, sc.Text())
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // the real environment wins
		}
		if err := os.Setenv(key, unquoteEnv(strings.TrimSpace(val))); err != nil {
			return set, fmt.Errorf("line %d: %w", line, err)
		}
		set++
	}
	return set, sc.Err()
}

func unquoteEnv(v string) string {
	if len(v) < 2 || v[0] != v[len(v)-1] {
		return v
	}
	switch v[0] {
	case '"':
		// Interpret \n and friends, but keep the raw text when the value is not
		// a valid Go string literal - a password is not required to be one.
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v[1 : len(v)-1]
	case '\'':
		return v[1 : len(v)-1] // single quotes are literal
	}
	return v
}

func LoadConfig() (*Config, error) {
	c := &Config{
		MatterWSURL:       envStr("MATTER_WS_URL", "ws://127.0.0.1:5580/ws"),
		MatterCallTimeout: envDuration("MATTER_CALL_TIMEOUT", 60*time.Second),
		MQTTBroker:        envStr("MQTT_BROKER", "tcp://127.0.0.1:1883"),
		MQTTUsername:      envStr("MQTT_USERNAME", ""),
		MQTTPassword:      envStr("MQTT_PASSWORD", ""),
		MQTTClientID:      envStr("MQTT_CLIENT_ID", "matter2mqtt"),
		MQTTQoS:           byte(envInt("MQTT_QOS", 0)),
		RetainState:       envBool("MQTT_RETAIN", true),
		TopicPrefix:       strings.TrimSuffix(envStr("MQTT_TOPIC_PREFIX", "matter"), "/"),
		PublishNames:      envBool("MQTT_PUBLISH_NAMES", true),
		ModelPath:         envStr("MATTER_MODEL_PATH", ""),
		HTTPEnabled:       envBool("HTTP_ENABLED", true),
		HTTPAddr:          envStr("HTTP_ADDR", "127.0.0.1:8099"),
		HTTPUsername:      envStr("HTTP_USERNAME", ""),
		HTTPPassword:      envStr("HTTP_PASSWORD", ""),
		LogLevel:          envStr("LOG_LEVEL", "info"),
	}

	if c.TopicPrefix == "" {
		return nil, fmt.Errorf("MQTT_TOPIC_PREFIX must not be empty")
	}
	if c.MQTTQoS > 2 {
		return nil, fmt.Errorf("MQTT_QOS must be 0, 1 or 2 (got %d)", c.MQTTQoS)
	}
	if !strings.HasPrefix(c.MatterWSURL, "ws://") && !strings.HasPrefix(c.MatterWSURL, "wss://") {
		return nil, fmt.Errorf("MATTER_WS_URL must start with ws:// or wss:// (got %q)", c.MatterWSURL)
	}
	// The console can commission and remove devices. Refuse to expose it
	// beyond loopback unless credentials are set.
	if c.HTTPEnabled && c.HTTPUsername == "" && !isLoopbackAddr(c.HTTPAddr) {
		return nil, fmt.Errorf(
			"HTTP_ADDR %q is not loopback: set HTTP_USERNAME and HTTP_PASSWORD, or bind to 127.0.0.1", c.HTTPAddr)
	}
	return c, nil
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
