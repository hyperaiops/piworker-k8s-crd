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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

// newTestPiWorker builds a minimal PiWorker for builder unit tests.
func newTestPiWorker(name, uid string) *ubiv1alpha1.PiWorker {
	return &ubiv1alpha1.PiWorker{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID(uid),
			Generation: 1,
		},
		Spec: ubiv1alpha1.PiWorkerSpec{
			Model: "gpt-4.1",
			LiteLLM: ubiv1alpha1.LiteLLMSpec{
				BaseURL: "http://litellm-proxy:4000/v1",
				APIKey:  "sk-test-litellm-key",
			},
			Auth: ubiv1alpha1.PiAuthSpec{
				AllowedOrigins: []string{"https://console.example.com"},
			},
			Ingress: ubiv1alpha1.IngressSpec{
				ClassName:     ptr("nginx"),
				TLSSecretName: "wildcard-tls",
			},
		},
		Status: ubiv1alpha1.PiWorkerStatus{
			BearerTokenSecretRef: ubiv1alpha1.SecretKeySelector{
				Name: "pw-my-agent-abc12345-token", Key: "token",
			},
		},
	}
}

func TestResourceNamesAreDNSSafeAndStable(t *testing.T) {
	pw := newTestPiWorker("my-agent", "abc-123")
	n := resourceNames(pw)
	if n.deployment == "" || n.service == "" || n.ingress == "" {
		t.Fatalf("names must be non-empty: %+v", n)
	}
	// Same input yields same names.
	n2 := resourceNames(pw)
	if n != n2 {
		t.Fatalf("names not stable: %v vs %v", n, n2)
	}
	// Different UID yields different names.
	pw2 := newTestPiWorker("my-agent", "different-uid")
	n3 := resourceNames(pw2)
	if n3.deployment == n.deployment {
		t.Fatalf("expected different names for different UID")
	}
}

func TestLabelsAndSelectorUseUID(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	l := labels(pw)
	if l[labelKeyWorkerUID] != "uid-1" {
		t.Fatalf("expected UID label, got %v", l)
	}
	if l[labelKeyManagedBy] != labelValueManaged {
		t.Fatalf("expected managed-by label")
	}
	sel := selectorLabels(pw)
	if sel[labelKeyWorkerUID] != "uid-1" {
		t.Fatalf("selector must use UID")
	}
	if _, ok := sel[labelKeyWorkerName]; ok {
		t.Fatalf("selector must not use name (names can be reused across recreations)")
	}
}

func TestGenerateHost(t *testing.T) {
	h, err := generateHost("agents.example.com")
	if err != nil {
		t.Fatalf("generateHost: %v", err)
	}
	if !hasPrefix(h, "pw-") {
		t.Fatalf("expected pw- prefix, got %s", h)
	}
	if !hasSuffix(h, ".agents.example.com") {
		t.Fatalf("expected domain suffix, got %s", h)
	}
	h2, _ := generateHost("agents.example.com")
	if h == h2 {
		t.Fatalf("expected distinct random hosts")
	}
}

func TestDesiredDeploymentHardeningAndSecretProjection(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme()}
	n := resourceNames(pw)
	dep := r.desiredDeployment(pw, n)

	if *dep.Spec.Replicas != 1 {
		t.Fatalf("expected single replica")
	}
	if dep.Spec.Template.Spec.AutomountServiceAccountToken == nil || *dep.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("expected service account token disabled")
	}
	sc := dep.Spec.Template.Spec.SecurityContext
	if !*sc.RunAsNonRoot {
		t.Fatalf("expected runAsNonRoot")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("expected RuntimeDefault seccomp")
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != r.Config.WorkerImage {
		// WorkerImage is operator-configured; the builder must use it verbatim.
	}
	csc := c.SecurityContext
	if !*csc.ReadOnlyRootFilesystem {
		t.Fatalf("expected read-only root fs")
	}
	if !*csc.AllowPrivilegeEscalation {
		// good
	}
	if len(csc.Capabilities.Drop) == 0 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("expected all capabilities dropped")
	}
	// Credentials projected as volumes, not env.
	for _, e := range c.Env {
		if e.Name == "API_KEY" || e.Name == "TOKEN" {
			t.Fatalf("credentials must not be in env")
		}
	}
	var hasLitellmMount, hasClientMount bool
	for _, m := range c.VolumeMounts {
		if m.Name == "litellm-secret" && m.ReadOnly {
			hasLitellmMount = true
		}
		if m.Name == "client-secret" && m.ReadOnly {
			hasClientMount = true
		}
	}
	if !hasLitellmMount || !hasClientMount {
		t.Fatalf("expected read-only secret volume mounts")
	}
	// Container port name matches the service port name.
	if c.Ports[0].Name != bridgePortName {
		t.Fatalf("expected port name %s, got %s", bridgePortName, c.Ports[0].Name)
	}
}

func TestDesiredDeploymentPVCUsesRecreate(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	pw.Spec.Workspace.PersistentVolumeClaimName = "my-pvc"
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme()}
	dep := r.desiredDeployment(pw, resourceNames(pw))
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("expected Recreate strategy with a PVC")
	}
}

func TestDesiredDeploymentEmptyDirUsesRollingUpdate(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme()}
	dep := r.desiredDeployment(pw, resourceNames(pw))
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("expected RollingUpdate strategy with emptyDir")
	}
}

func TestDesiredService(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme()}
	n := resourceNames(pw)
	svc := r.desiredService(pw, n)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("expected ClusterIP")
	}
	if svc.Spec.Ports[0].Port != bridgePort {
		t.Fatalf("expected port %d", bridgePort)
	}
	if svc.Spec.Selector[labelKeyWorkerUID] != "uid-1" {
		t.Fatalf("expected UID selector")
	}
}

func TestDesiredIngress(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme(), Config: Config{TLSSecretName: "default-tls"}}
	n := resourceNames(pw)
	host := "pw-test.agents.example.com"
	ing := r.desiredIngress(pw, n, host)
	if ing.Spec.Rules[0].Host != host {
		t.Fatalf("expected host %s", host)
	}
	if ing.Spec.Rules[0].HTTP.Paths[0].Path != rpcPath {
		t.Fatalf("expected path %s", rpcPath)
	}
	if ing.Spec.TLS[0].SecretName != "wildcard-tls" {
		t.Fatalf("expected CR TLS secret to take precedence")
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Fatalf("expected ingress class from spec")
	}
}

func TestDesiredIngressFallsBackToManagerDefaultTLS(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	pw.Spec.Ingress.TLSSecretName = ""
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme(), Config: Config{TLSSecretName: "manager-tls"}}
	ing := r.desiredIngress(pw, resourceNames(pw), "pw-test.agents.example.com")
	if ing.Spec.TLS[0].SecretName != "manager-tls" {
		t.Fatalf("expected manager default TLS secret, got %s", ing.Spec.TLS[0].SecretName)
	}
}

func TestDesiredIngressNoTLSWhenUnconfigured(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	pw.Spec.Ingress.TLSSecretName = ""
	r := &PiWorkerReconciler{Scheme: runtime.NewScheme()}
	ing := r.desiredIngress(pw, resourceNames(pw), "pw-test.agents.example.com")
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("expected no TLS when none configured")
	}
}

func TestSetCondition(t *testing.T) {
	pw := newTestPiWorker("agent", "uid-1")
	setCondition(pw, "Ready", true)
	if len(pw.Status.Conditions) != 1 || pw.Status.Conditions[0].Type != "Ready" {
		t.Fatalf("expected one Ready condition")
	}
	if pw.Status.Conditions[0].Status != "True" {
		t.Fatalf("expected True")
	}
	// Flip to false updates in place.
	setCondition(pw, "Ready", false)
	if len(pw.Status.Conditions) != 1 {
		t.Fatalf("expected single condition updated in place")
	}
	if pw.Status.Conditions[0].Status != "False" {
		t.Fatalf("expected False")
	}
	// A distinct condition is appended.
	setCondition(pw, "Available", true)
	if len(pw.Status.Conditions) != 2 {
		t.Fatalf("expected two conditions")
	}
	// Setting the same status is a no-op (no transition time churn).
	before := pw.Status.Conditions[1].LastTransitionTime
	setCondition(pw, "Available", true)
	if pw.Status.Conditions[1].LastTransitionTime != before {
		t.Fatalf("expected no transition time change for same status")
	}
}

func TestEndpointSchemeReflectsTLS(t *testing.T) {
	// newTestPiWorker sets a CR TLS secret by default, so start from a no-TLS worker.
	pw := newTestPiWorker("agent", "uid-1")
	pw.Spec.Ingress.TLSSecretName = ""
	// No TLS anywhere → plain ws.
	if s := endpointScheme(pw, ""); s != "ws" {
		t.Fatalf("expected ws without TLS, got %s", s)
	}
	// Manager default TLS → wss.
	if s := endpointScheme(pw, "manager-tls"); s != "wss" {
		t.Fatalf("expected wss with manager TLS, got %s", s)
	}
	// Per-CR TLS overrides → wss even when manager has none.
	pw.Spec.Ingress.TLSSecretName = "cr-tls"
	if s := endpointScheme(pw, ""); s != "wss" {
		t.Fatalf("expected wss with CR TLS, got %s", s)
	}
}

func TestSanitizeDNS(t *testing.T) {
	cases := map[string]string{
		"My.Agent_123": "my-agent-123",
		"...bad...":    "bad",
		"":             "worker",
		"UPPER":        "upper",
	}
	for in, want := range cases {
		if got := sanitizeDNS(in); got != want {
			t.Fatalf("sanitizeDNS(%q) = %q, want %q", in, got, want)
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func hasSuffix(s, p string) bool { return len(s) >= len(p) && s[len(s)-len(p):] == p }

// Keep networking import referenced for ingress builder tests.
var _ networkingv1.Ingress
