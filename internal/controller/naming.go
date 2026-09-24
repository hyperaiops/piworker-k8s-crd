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

package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

const (
	// labelKeyWorkerUID labels all child resources for a PiWorker.
	labelKeyWorkerUID = "ubi.pi.dev/piworker-uid"
	// labelKeyWorkerName carries the PiWorker name for readability.
	labelKeyWorkerName = "ubi.pi.dev/piworker-name"
	// labelKeyManagedBy marks resources managed by this operator.
	labelKeyManagedBy = "app.kubernetes.io/managed-by"
	labelValueManaged = "pi-agent-operator"

	// bridgePort is the container port the bridge HTTP server listens on.
	bridgePort = 8080
	// bridgePortName is the Service port name for the bridge.
	bridgePortName = "rpc"
	// rpcPath is the WebSocket path exposed through the Ingress.
	rpcPath = "/v1/rpc"
)

// names holds the deterministic names for a PiWorker's child resources.
type names struct {
	deployment string
	service    string
	ingress    string
}

// resourceNames derives DNS-1123-safe names from the PiWorker name and UID. A short UID
// hash suffix prevents a deleted/recreated CR with the same name from adopting stale
// resources.
func resourceNames(piworker *ubiv1alpha1.PiWorker) names {
	base := sanitizeDNS(piworker.Name)
	suffix := shortHash(string(piworker.UID))
	n := names{
		deployment: "pw-" + base + "-" + suffix,
		service:    "pw-" + base + "-" + suffix,
		ingress:    "pw-" + base + "-" + suffix,
	}
	// Keep each well under the 253-char DNS label limit.
	n.deployment = trunc(n.deployment, 63)
	n.service = trunc(n.service, 63)
	n.ingress = trunc(n.ingress, 253)
	return n
}

// labels returns the shared label set for all child resources.
func labels(piworker *ubiv1alpha1.PiWorker) map[string]string {
	return map[string]string{
		labelKeyWorkerUID:  string(piworker.UID),
		labelKeyWorkerName: piworker.Name,
		labelKeyManagedBy:  labelValueManaged,
	}
}

// endpointScheme returns the WebSocket URL scheme for the public endpoint: "wss" when TLS
// is configured (either per-CR or via the manager default), "ws" otherwise. This mirrors the
// TLS resolution in desiredIngress so .status.endpoint reflects what the Ingress serves.
func endpointScheme(piworker *ubiv1alpha1.PiWorker, managerTLSSecret string) string {
	if piworker.Spec.Ingress.TLSSecretName != "" || managerTLSSecret != "" {
		return "wss"
	}
	return "ws"
}

// generateHost produces a random hostname under domain. The prefix is 16 bytes of
// cryptographic randomness, base32-encoded (lowercase, no padding), and prefixed with "pw-".
func generateHost(domain string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return "pw-" + strings.ToLower(enc) + "." + domain, nil
}

// shortHash returns the first 8 hex chars of the SHA-256 of s, enough to disambiguate
// resources for a recreated CR with the same name.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func sanitizeDNS(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	s = strings.Trim(s, "-")
	if s == "" {
		s = "worker"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
