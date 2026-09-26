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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

// Config holds operator-wide settings injected from manager flags.
type Config struct {
	// WorkerImage is the operator-controlled worker image containing the bridge + Pi.
	WorkerImage string
	// WorkerImagePullSecret, when non-empty, is the name of a same-namespace Secret of
	// type kubernetes.io/dockerconfigjson the worker pod uses to pull WorkerImage. Lets
	// the controller deploy a private registry image without reading the Secret data.
	WorkerImagePullSecret string
	// IngressDomain is the wildcard base domain, e.g. "agents.example.com".
	IngressDomain string
	// TLSSecretName, when non-empty, is the default TLS secret used for the Ingress when the
	// CR does not specify one.
	TLSSecretName string
}

// desiredDeployment builds the worker Deployment for a PiWorker. Credentials are projected
// from Secrets as files; the controller never reads Secret data.
func (r *PiWorkerReconciler) desiredDeployment(piworker *ubiv1alpha1.PiWorker, n names) *appsv1.Deployment {
	labels := labels(piworker)
	replicas := int32(1)

	workerImage := piworker.Spec.WorkerImage
	if workerImage == "" {
		workerImage = r.Config.WorkerImage
	}
	workerImagePullSecret := piworker.Spec.WorkerImagePullSecret
	if workerImagePullSecret == "" {
		workerImagePullSecret = r.Config.WorkerImagePullSecret
	}

	volumes, mounts := workerVolumes(piworker, n)

	resources := piworker.Spec.Resources
	if resources.Requests == nil {
		resources.Requests = defaultResourceRequests()
	}
	if resources.Limits == nil {
		resources.Limits = defaultResourceLimits()
	}

	strategy := appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	if piworker.Spec.Workspace.PersistentVolumeClaimName == "" {
		strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		// Match the API-server defaults so the desired spec equals the live one and the
		// reconciler's merge patch is empty when there is no drift; otherwise the patch is
		// non-empty every reconcile, racing the Deployment controller and stalling
		// status.observedGeneration (Available never flips).
		twofive := intstr.FromString("25%")
		strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &twofive,
			MaxSurge:       &twofive,
		}
	}

	podSpec := corev1.PodSpec{
		AutomountServiceAccountToken: ptr(false),
		// Set the API-server defaults explicitly so the desired spec equals the live one
		// and the reconciler's merge patch is empty when there is no drift.
		RestartPolicy:                 corev1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: ptr(int64(30)),
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 "default-scheduler",
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr(int64(10001)),
			RunAsGroup:   ptr(int64(10001)),
			FSGroup:      ptr(int64(10001)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Volumes: volumes,
		Containers: []corev1.Container{
			{
				Name:  "bridge",
				Image: workerImage,
				// Match the API-server defaults so the desired spec equals the live one.
				ImagePullPolicy:          corev1.PullIfNotPresent,
				TerminationMessagePath:   "/dev/termination-log",
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				Args: []string{
					"--addr=:" + itoa(bridgePort),
					"--pi-binary=pi",
					"--model=" + piworker.Spec.Model,
					"--litellm-base-url=" + piworker.Spec.LiteLLM.BaseURL,
					"--api-key-path=/secrets/litellm/" + litellmAPIKeyKey,
					"--token-path=/secrets/client/" + bearerTokenKey,
					"--workspace-dir=/workspace",
					"--home-dir=/home/pi",
				},
				Ports: []corev1.ContainerPort{{
					Name:          bridgePortName,
					ContainerPort: bridgePort,
					Protocol:      corev1.ProtocolTCP,
				}},
				Resources: resources,
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr(true),
					AllowPrivilegeEscalation: ptr(false),
					RunAsNonRoot:             ptr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts:   mounts,
				LivenessProbe:  probe("/healthz"),
				ReadinessProbe: probe("/readyz"),
			},
		},
	}

	// Attach the image pull secret so the pod can pull a private worker image (e.g. ghcr.io).
	// The controller references the Secret by name only. A PiWorker may override both the
	// image and its pull secret; otherwise the manager defaults apply.
	if workerImagePullSecret != "" {
		podSpec.ImagePullSecrets = []corev1.LocalObjectReference{{
			Name: workerImagePullSecret,
		}}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      n.deployment,
			Namespace: piworker.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: strategy,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(piworker)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
			RevisionHistoryLimit:    ptr(int32(10)),
			ProgressDeadlineSeconds: ptr(int32(600)),
		},
	}
}

// desiredService builds the ClusterIP Service fronting the bridge.
func (r *PiWorkerReconciler) desiredService(piworker *ubiv1alpha1.PiWorker, n names) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      n.service,
			Namespace: piworker.Namespace,
			Labels:    labels(piworker),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(piworker),
			Ports: []corev1.ServicePort{{
				Name:       bridgePortName,
				Port:       bridgePort,
				TargetPort: intstr.FromString(bridgePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// desiredIngress builds the public Ingress exposing the bridge WebSocket endpoint.
func (r *PiWorkerReconciler) desiredIngress(piworker *ubiv1alpha1.PiWorker, n names, host string) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	tlsSecret := piworker.Spec.Ingress.TLSSecretName
	if tlsSecret == "" {
		tlsSecret = r.Config.TLSSecretName
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      n.ingress,
			Namespace: piworker.Namespace,
			Labels:    labels(piworker),
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     rpcPath,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: n.service,
									Port: networkingv1.ServiceBackendPort{
										Name: bridgePortName,
									},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if tlsSecret != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{host},
			SecretName: tlsSecret,
		}}
	}
	if piworker.Spec.Ingress.ClassName != nil && *piworker.Spec.Ingress.ClassName != "" {
		ing.Spec.IngressClassName = piworker.Spec.Ingress.ClassName
	}
	return ing
}

func workerVolumes(piworker *ubiv1alpha1.PiWorker, n names) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{
		workspaceVolume(piworker),
		{Name: "pi-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		litellmSecretVolume(n),
		clientSecretVolume(piworker),
	}
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "pi-home", MountPath: "/home/pi"},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "litellm-secret", MountPath: "/secrets/litellm", ReadOnly: true},
		{Name: "client-secret", MountPath: "/secrets/client", ReadOnly: true},
	}
	return volumes, mounts
}

func workspaceVolume(piworker *ubiv1alpha1.PiWorker) corev1.Volume {
	if pvc := piworker.Spec.Workspace.PersistentVolumeClaimName; pvc != "" {
		return corev1.Volume{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc,
				},
			},
		}
	}
	return corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func litellmSecretVolume(n names) corev1.Volume {
	return corev1.Volume{
		Name: "litellm-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: n.litellmSecret,
				// DefaultMode matches the API-server default (0644) so the desired spec
				// equals the live one and the reconciler's merge patch is empty when there
				// is no drift. Leaving it nil makes the patch non-empty every reconcile,
				// which races the Deployment controller and stalls status.observedGeneration.
				DefaultMode: ptr(int32(0644)),
				Items: []corev1.KeyToPath{{
					Key:  litellmAPIKeyKey,
					Path: litellmAPIKeyKey,
				}},
			},
		},
	}
}

func clientSecretVolume(piworker *ubiv1alpha1.PiWorker) corev1.Volume {
	return corev1.Volume{
		Name: "client-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  piworker.Status.BearerTokenSecretRef.Name,
				DefaultMode: ptr(int32(0644)),
				Items: []corev1.KeyToPath{{
					Key:  bearerTokenKey,
					Path: bearerTokenKey,
				}},
			},
		},
	}
}

// desiredTokenSecret builds the controller-owned Secret holding the auto-generated client
// bearer token. The token value is generated once and persisted; on subsequent reconciles
// the existing Secret data is preserved (the controller never overwrites an existing token).
func (r *PiWorkerReconciler) desiredTokenSecret(piworker *ubiv1alpha1.PiWorker, n names, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      n.tokenSecret,
			Namespace: piworker.Namespace,
			Labels:    labels(piworker),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			bearerTokenKey: token,
		},
	}
}

// desiredLiteLLMSecret builds the controller-owned Secret holding the LiteLLM API key. The
// key value comes from spec.litellm.apiKey (provided by the client at CR creation); the
// controller projects it into the worker Pod from this Secret. Because the Secret is owned
// by the PiWorker, it is garbage-collected when the CR is deleted.
func (r *PiWorkerReconciler) desiredLiteLLMSecret(piworker *ubiv1alpha1.PiWorker, n names) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      n.litellmSecret,
			Namespace: piworker.Namespace,
			Labels:    labels(piworker),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			litellmAPIKeyKey: piworker.Spec.LiteLLM.APIKey,
		},
	}
}

func selectorLabels(piworker *ubiv1alpha1.PiWorker) map[string]string {
	return map[string]string{
		labelKeyWorkerUID: string(piworker.UID),
	}
}

func probe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromString(bridgePortName),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		TimeoutSeconds:      1,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
}

func defaultResourceRequests() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	}
}

func defaultResourceLimits() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("2Gi"),
	}
}

func ptr[T any](v T) *T { return &v }

func itoa(n int) string { return fmt.Sprintf("%d", n) }
