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
	"context"
	"io"
	"os"
	"os/exec"
	"time"
)

// PiAgentConfig configures the Pi RPC subprocess launched by PiAgentFactory.
type PiAgentConfig struct {
	// Binary is the path to the `pi` executable.
	Binary string

	// Model is the LiteLLM model name passed to Pi and registered by the litellm extension.
	Model string

	// LiteLLMBaseURL is the OpenAI-compatible endpoint.
	LiteLLMBaseURL string

	// APIKeyPath is the filesystem path to the LiteLLM API key file.
	APIKeyPath string

	// WorkspaceDir is the working directory the agent runs in.
	WorkspaceDir string

	// HomeDir is the HOME / Pi state directory.
	HomeDir string

	// NoSession disables session persistence for headless RPC use.
	NoSession bool

	// Env is additional environment for the child (e.g. PATH). The bridge adds the
	// LiteLLM/OpenAI-compatible provider configuration.
	Env []string
}

// PiAgentFactory returns an AgentFactory bound to cfg that spawns `pi --mode rpc`.
func PiAgentFactory(cfg PiAgentConfig) AgentFactory {
	return func(ctx context.Context) (Agent, error) {
		args := []string{"--mode", "rpc"}
		if cfg.NoSession {
			args = append(args, "--no-session")
		}
		args = append(args, "--provider", "litellm")
		args = append(args, "--model", cfg.Model)

		cmd := exec.CommandContext(ctx, cfg.Binary, args...)
		cmd.Dir = cfg.WorkspaceDir

		env := append([]string{}, cfg.Env...)
		env = append(env, "HOME="+cfg.HomeDir)
		env = append(env, "PI_LITELLM_BASE_URL="+cfg.LiteLLMBaseURL)
		env = append(env, "PI_LITELLM_MODEL="+cfg.Model)
		env = append(env, "PI_LITELLM_API_KEY_FILE="+cfg.APIKeyPath)
		// The litellm extension reads the API key directly from the projected Secret file at
		// request time via PI_LITELLM_API_KEY_FILE, so the key never enters argv or env.
		cmd.Env = append(os.Environ(), env...)

		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}

		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &piAgent{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
	}
}

// piAgent wraps an exec.Cmd running Pi in RPC mode.
type piAgent struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (a *piAgent) Stdin() io.Writer  { return a.stdin }
func (a *piAgent) Stdout() io.Reader { return a.stdout }
func (a *piAgent) Stderr() io.Reader { return a.stderr }

// Stop closes stdin and terminates the subprocess: a graceful SIGTERM, then SIGKILL after
// a bounded wait. The cmd's context cancellation also applies via exec.CommandContext.
func (a *piAgent) Stop() error {
	_ = a.stdin.Close()
	if a.cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	if err := a.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = a.cmd.Process.Kill()
	}
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = a.cmd.Process.Kill()
		return <-done
	}
}
