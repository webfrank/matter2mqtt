package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed web/index.html
var consoleHTML []byte

// Server hosts the embedded web console. Every mutating route funnels through
// Bridge.Invoke, so the console and the MQTT interface share one code path.
type Server struct {
	cfg    *Config
	bridge *Bridge
	http   *http.Server
}

func NewServer(cfg *Config, b *Bridge) *Server {
	s := &Server{cfg: cfg, bridge: b}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/nodes", s.handleNodes)
	mux.HandleFunc("GET /api/model", s.handleModel)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/command", s.handleCommand)
	mux.HandleFunc("POST /api/write", s.handleWrite)
	mux.HandleFunc("POST /api/invoke", s.handleInvoke)

	s.http = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           s.withAuth(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func (s *Server) Start() error {
	ln, err := listen(s.cfg.HTTPAddr)
	if err != nil {
		return err
	}
	slog.Info("console listening",
		"addr", s.cfg.HTTPAddr,
		"auth", s.cfg.HTTPUsername != "")
	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("console server stopped", "err", err)
		}
	}()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) { _ = s.http.Shutdown(ctx) }

// withAuth applies HTTP basic auth when credentials are configured.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.HTTPUsername == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.HTTPUsername)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.HTTPPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="matter2mqtt"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- read routes ----------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.bridge.Status())
}

// nodeView pairs the raw node with its decoded endpoint map so the UI does not
// have to re-implement Descriptor parsing.
type nodeView struct {
	Node
	// Name is the named/ topic segment; Named is false when the device has no
	// NodeLabel and is therefore absent from the named/ tree.
	Name      string                  `json:"name"`
	Named     bool                    `json:"named"`
	Endpoints map[string]endpointInfo `json:"endpoints"`
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	nodes := s.bridge.Nodes()
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		name, named := s.bridge.namedKey(n.NodeID)
		out = append(out, nodeView{
			Node:      n,
			Name:      name,
			Named:     named,
			Endpoints: s.bridge.Endpoints(n),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleModel(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(s.bridge.Model().Raw())
}

// handleEvents streams bridge activity to the console as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.bridge.subscribeConsole()
	defer s.bridge.unsubscribeConsole(ch)

	if snapshot, err := json.Marshal(map[string]any{"type": "status", "data": s.bridge.Status()}); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", snapshot)
		flusher.Flush()
	}

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// ---------- mutating routes ----------

// handleCommand is the generic passthrough, mirroring the MQTT bridge/request
// topic. Everything matter-server exposes is reachable through it.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string         `json:"command"`
		Args    map[string]any `json:"args"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Command == "" {
		writeErr(w, http.StatusBadRequest, "command is required")
		return
	}
	s.run(w, r, req.Command, req.Args)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID    uint64 `json:"node_id"`
		Endpoint  int    `json:"endpoint"`
		Cluster   string `json:"cluster"`
		Attribute string `json:"attribute"`
		Value     any    `json:"value"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cluster, ok := s.bridge.Model().ResolveCluster(req.Cluster)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown cluster "+strconv.Quote(req.Cluster))
		return
	}
	attr, ok := s.bridge.Model().ResolveAttribute(cluster, req.Attribute)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown attribute "+strconv.Quote(req.Attribute))
		return
	}
	s.run(w, r, "write_attribute", map[string]any{
		"node_id":        req.NodeID,
		"attribute_path": fmt.Sprintf("%d/%d/%d", req.Endpoint, cluster, attr),
		"value":          req.Value,
	})
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID   uint64         `json:"node_id"`
		Endpoint int            `json:"endpoint"`
		Cluster  string         `json:"cluster"`
		Command  string         `json:"command"`
		Payload  map[string]any `json:"payload"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cluster, ok := s.bridge.Model().ResolveCluster(req.Cluster)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown cluster "+strconv.Quote(req.Cluster))
		return
	}
	if req.Command == "" {
		writeErr(w, http.StatusBadRequest, "command is required")
		return
	}
	if req.Payload == nil {
		req.Payload = map[string]any{}
	}
	s.run(w, r, "device_command", map[string]any{
		"node_id":      req.NodeID,
		"endpoint_id":  req.Endpoint,
		"cluster_id":   cluster,
		"command_name": req.Command,
		"payload":      req.Payload,
	})
}

// run executes a command and renders the result. Failures come back as 502
// with the matter-server error text intact rather than a generic message.
func (s *Server) run(w http.ResponseWriter, r *http.Request, command string, args map[string]any) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.MatterCallTimeout)
	defer cancel()

	result, err := s.bridge.Invoke(ctx, command, args)
	if err != nil {
		slog.Warn("console command failed", "command", command, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "command": command, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "command": command, "result": json.RawMessage(nonNil(result)),
	})
}

// ---------- helpers ----------

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

// listen is split out so tests can bind an ephemeral port.
func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}
