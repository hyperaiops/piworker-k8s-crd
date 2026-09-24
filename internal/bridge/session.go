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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/coder/websocket"
)

// serveSession runs one Pi RPC subprocess for the lifetime of the WebSocket connection.
// Inbound frames are validated and written to the agent stdin; agent stdout JSONL records
// are forwarded unchanged as text frames. The agent is terminated when the connection
// closes.
func (s *Server) serveSession(connCtx context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()

	agent, err := s.cfg.Agent(ctx)
	if err != nil {
		s.log.Error("failed to start agent", "err", err)
		_ = conn.Close(websocket.StatusInternalError, "agent start failed")
		return
	}
	defer func() {
		_ = agent.Stop()
	}()

	// stdout -> client. One JSONL record = one text frame.
	stdoutDone := make(chan struct{})
	go s.forwardAgentToClient(ctx, conn, agent, stdoutDone)

	// stderr -> sanitized logs only.
	stderrDone := make(chan struct{})
	go s.drainAgentStderr(agent, stderrDone)

	// client -> stdin. One text frame = one JSONL record.
	s.forwardClientToAgent(ctx, conn, agent)

	// The client disconnected (or the context canceled). Cancel the agent and wait for the
	// pump goroutines to observe end-of-stream so we never leak a subprocess.
	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "session ended")
	<-stdoutDone
	<-stderrDone
}

// forwardClientToAgent reads one WebSocket text message at a time, validates it is a single
// JSON object with no embedded newlines, and writes it as one JSONL record to the agent
// stdin. It blocks until the connection closes or the context is canceled.
func (s *Server) forwardClientToAgent(ctx context.Context, conn *websocket.Conn, agent Agent) {
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if msgType != websocket.MessageText {
			// Binary frames are protocol violations: a client must not be able to inject
			// arbitrary bytes that Pi would split on newlines.
			s.log.Warn("rejecting non-text websocket message")
			continue
		}
		if err := validateCommand(data); err != nil {
			s.log.Warn("rejecting inbound message", "err", err)
			continue
		}
		if _, err := agent.Stdin().Write(append(data, '\n')); err != nil {
			s.log.Debug("agent stdin write failed", "err", err)
			return
		}
	}
}

// forwardAgentToClient reads JSONL records from the agent stdout and writes each, unchanged,
// as one WebSocket text message. It returns when stdout reaches EOF or the context cancels.
func (s *Server) forwardAgentToClient(ctx context.Context, conn *websocket.Conn, agent Agent, done chan<- struct{}) {
	defer close(done)
	reader := bufio.NewReaderSize(agent.Stdout(), 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Pi guarantees one JSON record per line. Forward unmodified; drop the trailing
			// newline so the client receives the bare JSON object.
			record := line
			if record[len(record)-1] == '\n' {
				record = record[:len(record)-1]
			}
			if record[len(record)-1] == '\r' {
				record = record[:len(record)-1]
			}
			if err := conn.Write(ctx, websocket.MessageText, record); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// drainAgentStderr logs agent stderr line by line. Credentials are never placed in the agent
// command line or environment by the bridge, so no redaction is needed here; this is a
// defensive structured sink that keeps stderr out of the protocol stream.
func (s *Server) drainAgentStderr(agent Agent, done chan<- struct{}) {
	defer close(done)
	reader := bufio.NewReader(agent.Stderr())
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			msg := string(line)
			if l := len(msg); l > 0 && msg[l-1] == '\n' {
				msg = msg[:l-1]
			}
			s.log.Info("agent stderr", "line", msg)
		}
		if err != nil {
			return
		}
	}
}

// validateCommand ensures a single inbound frame is one JSON object with no embedded
// newlines, so it cannot be split into multiple JSONL records by Pi.
func validateCommand(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty message")
	}
	// The WebSocket read limit already bounds total message size.
	trimmed := trimSpace(data)
	if !json.Valid(trimmed) {
		return errors.New("invalid JSON")
	}
	for _, b := range trimmed {
		if b == '\n' || b == '\r' {
			return errors.New("embedded newline")
		}
	}
	// Require a JSON object: Pi commands are objects, not bare scalars/arrays.
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("not a JSON object")
	}
	return nil
}

func trimSpace(data []byte) []byte {
	i, j := 0, len(data)
	for i < j && isSpace(data[i]) {
		i++
	}
	for j > i && isSpace(data[j-1]) {
		j--
	}
	return data[i:j]
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

const maxCommandScan = 16 << 20

// Agent abstracts the Pi RPC subprocess so the session can be tested with a fake process.
type Agent interface {
	Stdin() io.Writer
	Stdout() io.Reader
	Stderr() io.Reader
	// Stop terminates the subprocess, first gracefully then forcibly after a bounded wait.
	Stop() error
}

// AgentFactory returns a running Agent bound to the given context.
type AgentFactory func(ctx context.Context) (Agent, error)
