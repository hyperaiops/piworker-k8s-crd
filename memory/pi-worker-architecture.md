---
name: pi-worker-architecture
description: How the PiWorker operator maps a CRD to a real Pi coding-agent RPC workload behind ingress
metadata:
  type: project
---

The PiWorker kubebuilder operator (`github.com/yfxue/pi-agent-operator`, group `ubi.pi.dev/v1alpha1`) turns each applied `PiWorker` CR into a real `pi --mode rpc` workload.

Key non-obvious facts:
- Pi's RPC protocol is **bidirectional JSONL over stdin/stdout**, NOT JSON-RPC/HTTP. The repo adds a Go bridge (`internal/bridge/` + `cmd/bridge`) that maps WebSocket text frames ↔ JSONL records transparently; it does not reinterpret Pi command/event schemas.
- Two images: `Dockerfile` (distroless controller manager) and `Dockerfile.worker` (bridge + pinned `@earendil-works/pi-coding-agent@0.85.1` + Node 22 + a baked-in LiteLLM OpenAI-compatible provider extension at `internal/bridge/pi-extension/litellm/`). The worker image is operator-controlled via manager flag `--worker-image`, not per-CR.
- Manager flags: `--worker-image` and `--ingress-domain` (required); `--ingress-tls-secret` (optional default). The controller generates a one-time random host `pw-<crypto-rand>.<ingress-domain>` stored in `.status.host`.
- LiteLLM is reached via OpenAI-compatible `spec.litellm.baseURL`; the API key is projected from a Secret as a file (controller never reads Secret data). Client auth to the public wss endpoint is a **separate** bearer-token Secret (`spec.auth.bearerTokenSecretRef`).
- `controllerutil` lives at `sigs.k8s.io/controller-runtime/pkg/controller/controllerutil` in controller-runtime v0.25 (not `pkg/controllerutil`).
- controller-gen installed at `bin/controller-gen-v0.22.0`; the `bin/controller-gen` symlink points outside the tree and the sandbox blocks running binaries via symlink — invoke `bin/controller-gen-v0.22.0` directly with sandbox disabled.
- envtest k8s binaries cannot be downloaded in this environment (GitHub binary fetch blocked); pure-Go controller unit tests live in `internal/controller/builders_test.go` and run without envtest. The Ginkgo envtest suite (`suite_test.go` + `piworker_controller_test.go`) compiles but requires `make setup-envtest` assets to run.
