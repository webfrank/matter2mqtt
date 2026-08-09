package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg *Config) (*Server, *Bridge) {
	t.Helper()
	m, err := LoadModel("")
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	if cfg == nil {
		cfg = &Config{TopicPrefix: "matter", MatterCallTimeout: 5 * time.Second, HTTPAddr: "127.0.0.1:0"}
	}
	b := NewBridge(cfg, m)
	return NewServer(cfg, b), b
}

// connect wires the bridge to a fake matter-server so command routes have a
// live session to talk to.
func connectFake(t *testing.T, b *Bridge) {
	t.Helper()
	srv := fakeServer(t)
	t.Cleanup(srv.Close)

	mc, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatalf("dial fake: %v", err)
	}
	t.Cleanup(mc.Close)

	b.mcMu.Lock()
	b.mc = mc
	b.mcMu.Unlock()
}

func TestConsoleServesIndexAndRejectsUnknownPaths(t *testing.T) {
	s, _ := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("index status %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("matter2mqtt console")) {
		t.Fatal("index does not look like the console")
	}

	rec = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown path status %d, want 404", rec.Code)
	}
}

func TestConsoleBasicAuth(t *testing.T) {
	cfg := &Config{
		TopicPrefix: "m", MatterCallTimeout: time.Second, HTTPAddr: "127.0.0.1:0",
		HTTPUsername: "ops", HTTPPassword: "s3cret",
	}
	s, _ := newTestServer(t, cfg)

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without credentials, got %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("ops", "wrong")
	rec = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with a bad password, got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("ops", "s3cret")
	rec = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with correct credentials, got %d", rec.Code)
	}
}

func TestConsoleRejectsNonLoopbackWithoutAuth(t *testing.T) {
	t.Setenv("HTTP_ADDR", "0.0.0.0:8099")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected config to refuse a public bind without credentials")
	}
	t.Setenv("HTTP_USERNAME", "ops")
	t.Setenv("HTTP_PASSWORD", "pw")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("credentials should permit a public bind: %v", err)
	}
}

func TestConsoleNodesIncludeDecodedEndpoints(t *testing.T) {
	s, b := newTestServer(t, nil)
	b.cacheNode(Node{NodeID: 4, Available: true, Attributes: map[string]json.RawMessage{
		"0/40/3":   json.RawMessage(`"Vallhorn"`),
		"1/29/0":   json.RawMessage(`[{"deviceType": 770, "revision": 2}]`),
		"1/29/1":   json.RawMessage(`[1026]`),
		"1/1026/0": json.RawMessage(`2137`),
	}})

	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nodes", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}

	var out []nodeView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].NodeID != 4 {
		t.Fatalf("unexpected nodes: %+v", out)
	}
	ep1 := out[0].Endpoints["1"]
	if len(ep1.DeviceTypes) != 1 || ep1.DeviceTypes[0].Name != "TemperatureSensor" {
		t.Fatalf("endpoints not decoded: %+v", ep1)
	}
}

func TestConsoleCommandRoutes(t *testing.T) {
	s, b := newTestServer(t, nil)
	connectFake(t, b)

	// Generic passthrough: the fake echoes args back as the result.
	body := `{"command":"echo","args":{"hello":"world"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/command", strings.NewReader(body))
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("command status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Success || !strings.Contains(string(out.Result), "world") {
		t.Fatalf("unexpected result: %s", rec.Body)
	}

	// Named cluster/attribute must resolve to numeric ids for matter-server.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/write",
		strings.NewReader(`{"node_id":4,"endpoint":1,"cluster":"LevelControl","attribute":"OnLevel","value":128}`))
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("write status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `1/8/17`) {
		t.Fatalf("attribute path not resolved from names: %s", rec.Body)
	}

	// Unknown names must fail fast with 400, not reach matter-server.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/write",
		strings.NewReader(`{"node_id":4,"endpoint":1,"cluster":"NoSuch","attribute":"X","value":1}`))
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown cluster, got %d", rec.Code)
	}
}

func TestConsoleReportsMatterErrors(t *testing.T) {
	s, b := newTestServer(t, nil)
	connectFake(t, b)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/command", strings.NewReader(`{"command":"boom"}`))
	s.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such node") {
		t.Fatalf("matter-server error text lost: %s", rec.Body)
	}
}

func TestConsoleCommandWithoutMatterConnection(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/command", strings.NewReader(`{"command":"get_nodes"}`))
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when disconnected, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("unhelpful error: %s", rec.Body)
	}
}

func TestConsoleEventStream(t *testing.T) {
	s, b := newTestServer(t, nil)

	ln, err := listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.http.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })

	url := "http://" + ln.Addr().String() + "/api/events"
	req, _ := http.NewRequest("GET", url, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	// First frame is the status snapshot.
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "status") {
		t.Fatalf("expected status snapshot, got %q (%v)", line, err)
	}

	// Subscribers registered, so a broadcast must reach this client.
	go func() {
		time.Sleep(100 * time.Millisecond)
		b.broadcast("attribute", map[string]any{"node_id": 4, "path": "1/1026/0"})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if strings.Contains(line, `"attribute"`) && strings.Contains(line, `1/1026/0`) {
			return
		}
	}
	t.Fatal("broadcast never arrived on the event stream")
}
