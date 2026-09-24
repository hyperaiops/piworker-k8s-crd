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

// Command bridge is the in-pod HTTP/WebSocket bridge to a Pi coding agent running in
// `--mode rpc`. It is packaged into the operator-controlled worker image, not the manager.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yfxue/pi-agent-operator/internal/bridge"
)

func main() {
	var (
		addr           string
		piBinary       string
		model          string
		litellmBaseURL string
		apiKeyPath     string
		tokenPath      string
		workspace      string
		homeDir        string
		agentDir       string
		extensionDir   string
		maxSessions    int
		maxMsgBytes    int64
		originsCSV     string
		envCSV         string
	)
	flag.StringVar(&addr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&piBinary, "pi-binary", "pi", "path to the pi executable")
	flag.StringVar(&model, "model", "", "LiteLLM model name")
	flag.StringVar(&litellmBaseURL, "litellm-base-url", "", "OpenAI-compatible LiteLLM endpoint")
	flag.StringVar(&apiKeyPath, "api-key-path", "", "path to the LiteLLM API key file")
	flag.StringVar(&tokenPath, "token-path", "", "path to the client bearer token file")
	flag.StringVar(&workspace, "workspace-dir", "/workspace", "agent working directory")
	flag.StringVar(&homeDir, "home-dir", "/home/pi", "agent HOME / state directory")
	flag.StringVar(&agentDir, "agent-dir", "", "Pi agent state directory (default <home>/.pi/agent); extensions are loaded from <dir>/extensions")
	flag.StringVar(&extensionDir, "extension-dir", "/opt/pi-extensions", "image-baked provider extensions linked into the writable agent dir")
	flag.IntVar(&maxSessions, "max-sessions", 1, "maximum concurrent agent sessions")
	flag.Int64Var(&maxMsgBytes, "max-message-bytes", 8<<20, "maximum inbound WebSocket message bytes")
	flag.StringVar(&originsCSV, "allowed-origins", "", "comma-separated allowed browser Origin values")
	flag.StringVar(&envCSV, "env", "", "comma-separated KEY=VALUE extra environment for the agent")
	flag.Parse()

	if model == "" || litellmBaseURL == "" || apiKeyPath == "" || tokenPath == "" {
		slog.Error("required flag missing", "model", model != "", "litellmBaseURL", litellmBaseURL != "",
			"apiKeyPath", apiKeyPath != "", "tokenPath", tokenPath != "")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var allowedOrigins []string
	if originsCSV != "" {
		for _, o := range strings.Split(originsCSV, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowedOrigins = append(allowedOrigins, o)
			}
		}
	}

	var extraEnv []string
	if envCSV != "" {
		for _, e := range strings.Split(envCSV, ",") {
			if e = strings.TrimSpace(e); e != "" {
				extraEnv = append(extraEnv, e)
			}
		}
	}

	// Pi's agent dir defaults to <HOME>/.pi/agent. Derive it from home-dir so the bridge links
	// extensions into exactly the location Pi scans, regardless of the bridge process's own HOME.
	if agentDir == "" {
		agentDir = homeDir + "/.pi/agent"
	}
	// Tell Pi to use the same agent dir explicitly so extension discovery is independent of the
	// child's HOME resolution.
	extraEnv = append(extraEnv, "PI_CODING_AGENT_DIR="+agentDir)

	agentFactory := bridge.PiAgentFactory(bridge.PiAgentConfig{
		Binary:         piBinary,
		Model:          model,
		LiteLLMBaseURL: litellmBaseURL,
		APIKeyPath:     apiKeyPath,
		WorkspaceDir:   workspace,
		HomeDir:        homeDir,
		NoSession:      true,
		Env:            extraEnv,
	})

	srv, err := bridge.NewServer(bridge.Config{
		Addr:            addr,
		Agent:           agentFactory,
		TokenPath:       tokenPath,
		AllowedOrigins:  allowedOrigins,
		MaxSessions:     maxSessions,
		MaxMessageBytes: maxMsgBytes,
		AgentDir:        agentDir,
		ExtensionDir:    extensionDir,
		Logger:          logger,
	})
	if err != nil {
		logger.Error("failed to create bridge server", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("bridge server stopped", "err", err)
		os.Exit(1)
	}
}
