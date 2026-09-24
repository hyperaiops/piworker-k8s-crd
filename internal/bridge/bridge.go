/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package bridge exposes a Pi coding agent running in `--mode rpc` over an authenticated
// WebSocket endpoint. The bridge is a thin transport: one inbound WebSocket text frame is
// one Pi command JSON object, written as a single JSONL record to the agent's stdin; each
// JSONL record emitted on the agent's stdout becomes one outbound WebSocket text frame.
// The bridge does not interpret Pi command or event schemas beyond JSON-object framing.
package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// Config configures the bridge server.
type Config struct {
	// Addr is the listen address for the HTTP server, e.g. ":8080".
	Addr string

	// Agent is the Pi subprocess factory used to start a new agent session.
	Agent AgentFactory

	// TokenPath is the filesystem path to the client bearer token file. The token is read
	// at connection time so projected-Secret rotation applies to new sessions.
	TokenPath string

	// AllowedOrigins restricts browser connections to these Origin values. Non-browser
	// clients without an Origin header are always allowed. Empty means no browser clients.
	AllowedOrigins []string

	// MaxSessions is the maximum number of concurrent active sessions. Each Pi session is
	// stateful and owns a single subprocess; independent client command streams would
	// interleave, so the default is one.
	MaxSessions int

	// MaxMessageBytes caps the size of a single inbound WebSocket message.
	MaxMessageBytes int64

	// AgentDir is Pi's writable agent state directory ($HOME/.pi/agent by default). Pi writes
	// auth.json/settings.json/sessions/tmp here and discovers extensions under <dir>/extensions.
	AgentDir string

	// ExtensionDir, when non-empty, is a read-only image-baked directory of provider
	// extensions (e.g. litellm) that must be linked into the writable AgentDir so Pi can load
	// them despite the runtime emptyDir shadowing the image's home. Each immediate child of
	// ExtensionDir is symlinked into <AgentDir>/extensions/<name>.
	ExtensionDir string

	// Logger receives structured log output. Pi stderr is sanitized and routed here.
	Logger *slog.Logger
}

// Server is the authenticated WebSocket-to-Pi bridge HTTP server.
type Server struct {
	cfg        Config
	allowed    map[string]struct{}
	log        *slog.Logger
	sessionsMu sync.Mutex
	active     int
	ready      atomicReady
}

// NewServer constructs a bridge Server. It validates config and applies safe defaults.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("bridge: Addr is required")
	}
	if cfg.Agent == nil {
		return nil, errors.New("bridge: Agent factory is required")
	}
	if cfg.TokenPath == "" {
		return nil, errors.New("bridge: TokenPath is required")
	}
	if cfg.MaxSessions < 1 {
		cfg.MaxSessions = 1
	}
	if cfg.MaxMessageBytes < 1 {
		cfg.MaxMessageBytes = 8 << 20 // 8 MiB default
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = struct{}{}
		}
	}
	if err := linkExtensions(cfg.AgentDir, cfg.ExtensionDir); err != nil {
		return nil, fmt.Errorf("bridge: link extensions: %w", err)
	}
	return &Server{cfg: cfg, allowed: allowed, log: cfg.Logger}, nil
}

// linkExtensions symlinks each immediate child of extensionDir into <agentDir>/extensions so
// Pi discovers image-baked provider extensions (litellm) despite the writable emptyDir mounted
// over the image's home. It is a no-op when extensionDir is empty. Existing links or real dirs
// at the target are left in place; only missing entries are created. The agent extensions
// directory is created if needed (the emptyDir home is writable by the bridge user). Each linked
// extension is also registered in <agentDir>/settings.json so Pi auto-loads it; Pi only
// discovers extensions listed there (and under extensions/), and the emptyDir starts empty.
func linkExtensions(agentDir, extensionDir string) error {
	if extensionDir == "" {
		return nil
	}
	if agentDir == "" {
		agentDir = defaultAgentDir()
	}
	targetRoot := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", targetRoot, err)
	}
	entries, err := os.ReadDir(extensionDir)
	if err != nil {
		return fmt.Errorf("read extension dir %s: %w", extensionDir, err)
	}
	var packages []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(extensionDir, e.Name())
		dst := filepath.Join(targetRoot, e.Name())
		if _, err := os.Lstat(dst); err == nil {
			// Already linked or present; leave a pre-existing entry untouched.
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dst, err)
		} else if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("link %s -> %s: %w", dst, src, err)
		}
		packages = append(packages, "extensions/"+e.Name())
	}
	if len(packages) == 0 {
		return nil
	}
	return ensurePackageSettings(agentDir, packages)
}

// ensurePackageSettings merges the given package paths into <agentDir>/settings.json, creating
// the file if absent. It preserves any other keys already present and avoids duplicate entries.
func ensurePackageSettings(agentDir string, packages []string) error {
	settingsPath := filepath.Join(agentDir, "settings.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", settingsPath, err)
	}
	raw, _ := settings["packages"].([]any)
	have := make(map[string]struct{}, len(raw))
	for _, p := range raw {
		if s, ok := p.(string); ok {
			have[s] = struct{}{}
		}
	}
	changed := false
	merged := append([]any{}, raw...)
	for _, p := range packages {
		if _, ok := have[p]; ok {
			continue
		}
		have[p] = struct{}{}
		merged = append(merged, p)
		changed = true
	}
	if !changed {
		return nil
	}
	settings["packages"] = merged
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", settingsPath, err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}
	return nil
}

// defaultAgentDir returns Pi's default agent dir ($HOME/.pi/agent). It mirrors Pi's own
// getAgentDir() fallback when PI_CODING_AGENT_DIR is unset.
func defaultAgentDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/home/pi"
	}
	return filepath.Join(home, ".pi", "agent")
}

// ListenAndServe starts the HTTP server. It blocks until the context is canceled or the
// server fails. Health and readiness probes are served on the same listener.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/v1/rpc", s.handleRPC)

	srv := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: mux,
		// Long-lived coding-agent sessions; the per-connection deadlines are managed on
		// the WebSocket reader/writer, not the HTTP server.
		ReadHeaderTimeout: 0,
	}

	s.ready.set(true)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.log.Info("bridge listening", "addr", s.cfg.Addr, "max_sessions", s.cfg.MaxSessions)
	err := srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const shutdownTimeout = 10

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.ready.get() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(w, r) {
		return
	}
	if !s.checkOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	if !s.acquireSession() {
		http.Error(w, "agent busy", http.StatusConflict)
		return
	}
	defer s.releaseSession()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin checks are enforced explicitly above for browsers; coder/websocket also
		// performs Origin checking, so mirror the policy here.
		OriginPatterns: originPatterns(s.allowed),
	})
	if err != nil {
		s.log.Warn("websocket accept failed", "err", err)
		return
	}
	// Long-lived coding sessions: disable the library default read deadline so a session
	// may idle while the agent runs tools. Per-message deadlines are applied by the session.
	conn.SetReadLimit(s.cfg.MaxMessageBytes)

	s.serveSession(r.Context(), conn)
}

// authenticate validates the bearer token against the mounted token file in constant time.
// The token file is re-read on every connection so Secret rotation takes effect for new
// sessions without a rollout.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authorization required", http.StatusUnauthorized)
		return false
	}
	presented := strings.TrimSpace(header[len(prefix):])
	expected, err := os.ReadFile(s.cfg.TokenPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.log.Error("client token file missing", "path", s.cfg.TokenPath)
		} else {
			s.log.Error("failed to read client token file", "err", err)
		}
		http.Error(w, "authorization unavailable", http.StatusServiceUnavailable)
		return false
	}
	expected = []byte(strings.TrimSpace(string(expected)))
	if subtle.ConstantTimeCompare([]byte(presented), expected) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// checkOrigin rejects browser requests whose Origin is not allow-listed. Requests without
// an Origin header (typical for CLI clients) are allowed.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if len(s.allowed) == 0 {
		return false
	}
	_, ok := s.allowed[origin]
	return ok
}

func (s *Server) acquireSession() bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.active >= s.cfg.MaxSessions {
		return false
	}
	s.active++
	return true
}

func (s *Server) releaseSession() {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.active > 0 {
		s.active--
	}
}

func originPatterns(allowed map[string]struct{}) []string {
	out := make([]string, 0, len(allowed))
	for o := range allowed {
		out = append(out, o)
	}
	return out
}
