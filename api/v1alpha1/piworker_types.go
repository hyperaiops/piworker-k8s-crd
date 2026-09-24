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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SecretKeySelector references a key in a same-namespace Secret. The controller never reads
// the Secret value; kubelet projects it into the worker Pod.
type SecretKeySelector struct {
	// name of the Secret holding the credential.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// key inside the Secret that holds the credential value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// LiteLLMSpec configures the OpenAI-compatible LiteLLM proxy the Pi agent routes model
// traffic through. The API key is projected from a Secret as a file and never read by the
// controller.
type LiteLLMSpec struct {
	// baseURL is the OpenAI-compatible LiteLLM endpoint, e.g. http://litellm-proxy:4000/v1.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	BaseURL string `json:"baseURL"`

	// apiKeySecretRef references a same-namespace Secret/key holding the LiteLLM API key.
	// +kubebuilder:validation:Required
	APIKeySecretRef SecretKeySelector `json:"apiKeySecretRef"`
}

// PiAuthSpec configures client authentication to the bridge's public WebSocket endpoint.
// The bearer token is distinct from the LiteLLM credential: it gates who may drive the
// agent over the exposed Ingress URL.
type PiAuthSpec struct {
	// bearerTokenSecretRef references a same-namespace Secret/key holding the high-entropy
	// bearer token clients must present as `Authorization: Bearer <token>` before the
	// WebSocket upgrade.
	// +kubebuilder:validation:Required
	BearerTokenSecretRef SecretKeySelector `json:"bearerTokenSecretRef"`

	// allowedOrigins restricts browser WebSocket connections to these Origin values.
	// Non-browser CLI clients without an Origin header are always allowed. When empty,
	// browser connections are rejected.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
}

// IngressSpec describes how the agent is exposed. The random hostname prefix is generated
// by the controller and combined with the manager-configured wildcard domain.
type IngressSpec struct {
	// className is the IngressClass name to use. When omitted the cluster default applies.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ClassName *string `json:"className,omitempty"`

	// tlsSecretName is the name of a same-namespace TLS Secret covering the wildcard domain.
	// Required when the manager requires TLS for the public endpoint.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	TLSSecretName string `json:"tlsSecretName,omitempty"`
}

// WorkspaceSpec describes the agent's working directory. Defaults to an ephemeral emptyDir
// that is erased when the Pod is recreated.
type WorkspaceSpec struct {
	// persistentVolumeClaimName, when set, mounts an existing same-namespace PVC at
	// /workspace for durable work. When omitted an emptyDir is used.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`
}

// PiWorkerSpec defines the desired state of PiWorker
type PiWorkerSpec struct {
	// model is the LiteLLM model name the agent uses, e.g. "gpt-4.1" or "claude-sonnet-4".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`

	// litellm configures the OpenAI-compatible LiteLLM endpoint and credentials.
	// +kubebuilder:validation:Required
	LiteLLM LiteLLMSpec `json:"litellm"`

	// auth configures client authentication to the public agent endpoint.
	// +kubebuilder:validation:Required
	Auth PiAuthSpec `json:"auth"`

	// ingress configures the public exposure of the agent.
	// +optional
	Ingress IngressSpec `json:"ingress,omitempty"`

	// workspace configures the agent working directory.
	// +optional
	Workspace WorkspaceSpec `json:"workspace,omitempty"`

	// workerImage overrides the operator-configured worker image for this PiWorker. When
	// omitted, the manager --worker-image default applies. Setting this lets a PiWorker
	// author run an arbitrary container image in the agent Pod, so the PiWorker namespace
	// must be trusted.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	WorkerImage string `json:"workerImage,omitempty"`

	// workerImagePullSecret, when set together with workerImage, is the name of a
	// same-namespace Secret of type kubernetes.io/dockerconfigjson the worker Pod uses to
	// pull workerImage from a private registry. When omitted the manager
	// --worker-image-pull-secret default applies.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	WorkerImagePullSecret string `json:"workerImagePullSecret,omitempty"`

	// resources optionally overrides compute resources for the worker Pod. The controller
	// applies conservative defaults when omitted.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PiWorkerStatus defines the observed state of PiWorker.
type PiWorkerStatus struct {
	// observedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// host is the generated public hostname, e.g. pw-<token>.<domain>.
	// +optional
	Host string `json:"host,omitempty"`

	// endpoint is the full public WebSocket URL clients connect to.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// workloadName is the name of the managed Deployment.
	// +optional
	WorkloadName string `json:"workloadName,omitempty"`

	// serviceName is the name of the managed Service.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// ingressName is the name of the managed Ingress.
	// +optional
	IngressName string `json:"ingressName,omitempty"`

	// conditions represent the current state of the PiWorker resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=piw
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.host`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=10

// PiWorker is the Schema for the piworkers API
type PiWorker struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PiWorker
	// +required
	Spec PiWorkerSpec `json:"spec"`

	// status defines the observed state of PiWorker
	// +optional
	Status PiWorkerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PiWorkerList contains a list of PiWorker
type PiWorkerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PiWorker `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PiWorker{}, &PiWorkerList{})
		return nil
	})
}
