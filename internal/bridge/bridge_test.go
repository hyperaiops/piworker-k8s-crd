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

package bridge

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeAgent is an in-memory Agent. stdin writes are echoed as JSONL records to stdout after
// the agent is "started", so a test can verify end-to-end framing without a real process.
type fakeAgent struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	stderr *io.PipeReader

	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter
	stdinR  *io.PipeReader

	stopOnce sync.Once
	stopErr  error
}

func newFakeAgent(echo bool) *fakeAgent {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	a := &fakeAgent{
		stdin:   stdinW,
		stdout:  stdoutR,
		stderr:  stderrR,
		stdinR:  stdinR,
		stdoutW: stdoutW,
		stderrW: stderrW,
	}
	if echo {
		go a.echoLoop()
	}
	return a
}

func (a *fakeAgent) echoLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := a.stdinR.Read(buf)
		if n > 0 {
			// Echo each received JSONL record back to stdout unchanged.
			a.stdoutW.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (a *fakeAgent) Stdin() io.Writer  { return a.stdin }
func (a *fakeAgent) Stdout() io.Reader { return a.stdout }
func (a *fakeAgent) Stderr() io.Reader { return a.stderr }

func (a *fakeAgent) Stop() error {
	a.stopOnce.Do(func() {
		a.stdin.Close()
		a.stdinR.Close()
		a.stdoutW.Close()
		a.stderrW.Close()
		a.stopErr = a.stderrW.Close()
	})
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, agent AgentFactory, token string, opts func(*Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	tokenPath := dir + "/token"
	if err := writeFile(tokenPath, token); err != nil {
		t.Fatalf("write token: %v", err)
	}
	cfg := Config{
		Addr:            "127.0.0.1:0",
		Agent:           agent,
		TokenPath:       tokenPath,
		MaxSessions:     1,
		MaxMessageBytes: 1 << 20,
		Logger:          discardLogger(),
	}
	if opts != nil {
		opts(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestAuthenticateRejectsMissingToken(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	rr := httptest.NewRecorder()
	srv.authenticate(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthenticateRejectsBadToken(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	if srv.authenticate(rr, req) {
		t.Fatalf("expected auth failure")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestAuthenticateAcceptsGoodToken(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	if !srv.authenticate(rr, req) {
		t.Fatalf("expected auth success")
	}
}

func TestOriginAllowed(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", func(c *Config) {
		c.AllowedOrigins = []string{"https://console.example.com"}
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req.Header.Set("Origin", "https://console.example.com")
	if !srv.checkOrigin(req) {
		t.Fatalf("expected allowed origin")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req2.Header.Set("Origin", "https://evil.example.com")
	if srv.checkOrigin(req2) {
		t.Fatalf("expected rejected origin")
	}
	// No Origin header: CLI clients always allowed.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	if !srv.checkOrigin(req3) {
		t.Fatalf("expected no-origin allowed")
	}
}

func TestOriginRejectedWhenNoneConfigured(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	if srv.checkOrigin(req) {
		t.Fatalf("expected browser origin rejected when none configured")
	}
}

func TestValidateCommand(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"object", `{"type":"get_state"}`, false},
		{"object with id", `{"id":"req-1","type":"prompt","message":"hi"}`, false},
		{"empty", "", true},
		{"array", `[]`, true},
		{"number", `42`, true},
		{"malformed", `{not json`, true},
		{"embedded newline", "{\n\"type\":\"get_state\"}", true},
		{"with spaces", `  {"type":"get_state"}  `, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateCommand([]byte(c.in))
			if (err != nil) != c.wantErr {
				t.Fatalf("got err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestSessionForwardsJSONL(t *testing.T) {
	agent := newFakeAgent(true)
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return agent, nil }, "secret", nil)

	hs := httptest.NewServer(http.HandlerFunc(srv.handleRPC))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/rpc"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: authHeader("secret"),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	cmd := []byte(`{"id":"req-1","type":"get_state"}`)
	if err := conn.Write(ctx, websocket.MessageText, cmd); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, cmd) {
		t.Fatalf("expected echo %q, got %q", cmd, data)
	}
}

func TestSessionRejectsBinary(t *testing.T) {
	agent := newFakeAgent(true)
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return agent, nil }, "secret", nil)

	hs := httptest.NewServer(http.HandlerFunc(srv.handleRPC))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/rpc"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: authHeader("secret"),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	if err := conn.Write(ctx, websocket.MessageBinary, []byte("raw")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A valid command should still be forwarded after the rejected binary frame.
	cmd := []byte(`{"type":"get_state"}`)
	if err := conn.Write(ctx, websocket.MessageText, cmd); err != nil {
		t.Fatalf("write text: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, cmd) {
		t.Fatalf("expected forwarded %q, got %q", cmd, data)
	}
}

func TestSessionLimitOne(t *testing.T) {
	// First session blocks forever (no echo, stdin never closed).
	blocker := newFakeAgent(false)
	var startedMu sync.Mutex
	started := 0
	factory := func(ctx context.Context) (Agent, error) {
		startedMu.Lock()
		started++
		startedMu.Unlock()
		return blocker, nil
	}
	srv := newTestServer(t, factory, "secret", nil)

	hs := httptest.NewServer(http.HandlerFunc(srv.handleRPC))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/rpc"
	c1, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: authHeader("secret"),
	})
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "done")

	// Second connection should be rejected with 409 before upgrade.
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/rpc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestAuthenticateNoTokenFile(t *testing.T) {
	srv := newTestServer(t, func(ctx context.Context) (Agent, error) { return newFakeAgent(false), nil }, "secret", nil)
	// Point at a missing path.
	srv.cfg.TokenPath = "/nonexistent/token"
	req := httptest.NewRequest(http.MethodGet, "/v1/rpc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	if srv.authenticate(rr, req) {
		t.Fatalf("expected failure when token file missing")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}

func TestNewServerValidation(t *testing.T) {
	if _, err := NewServer(Config{Addr: ":8080"}); err == nil {
		t.Fatalf("expected error without agent factory")
	}
	if _, err := NewServer(Config{Addr: ":8080", Agent: func(context.Context) (Agent, error) { return nil, nil }}); err == nil {
		t.Fatalf("expected error without token path")
	}
}

// authHeader returns an http.Header carrying a Bearer token.
func authHeader(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

func TestLinkExtensionsSymlinksBakedDir(t *testing.T) {
	// Simulate the image-baked extension dir (read-only source).
	src := t.TempDir()
	if err := os.MkdirAll(src+"/litellm", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+"/litellm/package.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Writable agent dir mirrors the runtime emptyDir home.
	agentDir := t.TempDir()

	if err := linkExtensions(agentDir, src); err != nil {
		t.Fatalf("link: %v", err)
	}
	link := agentDir + "/extensions/litellm"
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a symlink, got %v", info.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != src+"/litellm" {
		t.Fatalf("expected target %s, got %s", src+"/litellm", target)
	}
	// The baked content is reachable through the link.
	if _, err := os.Stat(link + "/package.json"); err != nil {
		t.Fatalf("package.json not reachable through symlink: %v", err)
	}

	// Re-running must be idempotent: an existing link is left in place.
	if err := linkExtensions(agentDir, src); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
}

func TestLinkExtensionsNoOpWhenEmpty(t *testing.T) {
	if err := linkExtensions(t.TempDir(), ""); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

func TestLinkExtensionsMissingSourceFails(t *testing.T) {
	if err := linkExtensions(t.TempDir(), "/no/such/extension/dir"); err == nil {
		t.Fatalf("expected error for missing extension dir")
	}
}
