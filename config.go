package main

import (
	"fmt"
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
