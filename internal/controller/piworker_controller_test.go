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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

const (
	testWorkerImage   = "piworker-worker:test"
	testIngressDomain = "agents.test.example.com"
	testTLSSecret     = "wildcard-tls"
)

var _ = Describe("PiWorker Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PiWorker")
			piworker := &ubiv1alpha1.PiWorker{}
			err := k8sClient.Get(ctx, typeNamespacedName, piworker)
			if err != nil && errors.IsNotFound(err) {
				resource := &ubiv1alpha1.PiWorker{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
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
							TLSSecretName: testTLSSecret,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &ubiv1alpha1.PiWorker{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleanup the specific resource instance PiWorker")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should successfully reconcile the resource and create child resources", func() {
			By("Reconciling the created resource")
			controllerReconciler := &PiWorkerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Config: Config{
					WorkerImage:   testWorkerImage,
					IngressDomain: testIngressDomain,
					TLSSecretName: testTLSSecret,
				},
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("reading back the PiWorker to inspect generated status")
			piworker := &ubiv1alpha1.PiWorker{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, piworker)).To(Succeed())
			Expect(piworker.Status.Host).To(HavePrefix("pw-"))
			Expect(piworker.Status.Host).To(HaveSuffix("." + testIngressDomain))
			Expect(piworker.Status.Endpoint).To(HavePrefix("wss://"))
			Expect(piworker.Status.Endpoint).To(HaveSuffix("/v1/rpc"))
			Expect(piworker.Status.WorkloadName).NotTo(BeEmpty())
			Expect(piworker.Status.ServiceName).NotTo(BeEmpty())
			Expect(piworker.Status.IngressName).NotTo(BeEmpty())

			By("verifying the controller auto-generated a bearer token Secret and recorded its ref")
			Expect(piworker.Status.BearerTokenSecretRef.Name).NotTo(BeEmpty())
			Expect(piworker.Status.BearerTokenSecretRef.Key).To(Equal("token"))
			tokenSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      piworker.Status.BearerTokenSecretRef.Name,
				Namespace: resourceNamespace,
			}, tokenSecret)).To(Succeed())
			Expect(tokenSecret.Data).To(HaveKey("token"))
			Expect(len(tokenSecret.Data["token"])).To(BeNumerically(">", 0))

			By("verifying the Deployment was created with the controlled worker image")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      piworker.Status.WorkloadName,
				Namespace: resourceNamespace,
			}, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(testWorkerImage))
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Spec.Template.Spec.AutomountServiceAccountToken).To(Equal(ptr(false)))
			sc := dep.Spec.Template.Spec.SecurityContext
			Expect(*sc.RunAsNonRoot).To(BeTrue())
			Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			csc := dep.Spec.Template.Spec.Containers[0].SecurityContext
			Expect(*csc.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*csc.AllowPrivilegeEscalation).To(BeFalse())
			Expect(csc.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			By("verifying credentials are projected as Secret volumes, not env values")
			var hasSecretEnv bool
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "API_KEY" || e.Name == "TOKEN" {
					hasSecretEnv = true
				}
			}
			Expect(hasSecretEnv).To(BeFalse())
			Expect(dep.Spec.Template.Spec.Containers[0].VolumeMounts).To(
				ContainElement(corev1.VolumeMount{Name: "litellm-secret", MountPath: "/secrets/litellm", ReadOnly: true}))
			Expect(dep.Spec.Template.Spec.Containers[0].VolumeMounts).To(
				ContainElement(corev1.VolumeMount{Name: "client-secret", MountPath: "/secrets/client", ReadOnly: true}))

			By("verifying the Service was created")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      piworker.Status.ServiceName,
				Namespace: resourceNamespace,
			}, svc)).To(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))

			By("verifying the Ingress was created with the generated host")
			ing := &networkingv1.Ingress{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      piworker.Status.IngressName,
				Namespace: resourceNamespace,
			}, ing)).To(Succeed())
			Expect(ing.Spec.Rules).To(HaveLen(1))
			Expect(ing.Spec.Rules[0].Host).To(Equal(piworker.Status.Host))
			Expect(ing.Spec.Rules[0].HTTP.Paths[0].Path).To(Equal("/v1/rpc"))
			Expect(ing.Spec.TLS).To(HaveLen(1))
			Expect(ing.Spec.TLS[0].SecretName).To(Equal(testTLSSecret))

			By("verifying owner references link children to the PiWorker")
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(dep.OwnerReferences[0].UID).To(Equal(piworker.UID))
			Expect(svc.OwnerReferences).To(HaveLen(1))
			Expect(ing.OwnerReferences).To(HaveLen(1))

			By("verifying a second reconcile is idempotent and keeps the host stable")
			prevHost := piworker.Status.Host
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, piworker)).To(Succeed())
			Expect(piworker.Status.Host).To(Equal(prevHost))

			// Avoid the controller-runtime requeue on requeue:true by waiting briefly.
			_ = result
		})

		It("should set a container port matching the Service target", func() {
			controllerReconciler := &PiWorkerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Config: Config{WorkerImage: testWorkerImage, IngressDomain: testIngressDomain},
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			piworker := &ubiv1alpha1.PiWorker{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, piworker)).To(Succeed())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: piworker.Status.ServiceName, Namespace: resourceNamespace,
			}, svc)).To(Succeed())
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: piworker.Status.WorkloadName, Namespace: resourceNamespace,
			}, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].Ports[0].Name).To(Equal(svc.Spec.Ports[0].TargetPort.String()))
			Expect(intstr.FromString(dep.Spec.Template.Spec.Containers[0].Ports[0].Name)).To(Equal(svc.Spec.Ports[0].TargetPort))
		})
	})

	Context("host generation", func() {
		It("produces unique random hosts under the domain", func() {
			h1, err := generateHost(testIngressDomain)
			Expect(err).NotTo(HaveOccurred())
			h2, err := generateHost(testIngressDomain)
			Expect(err).NotTo(HaveOccurred())
			Expect(h1).To(HavePrefix("pw-"))
			Expect(h1).To(HaveSuffix("." + testIngressDomain))
			Expect(h1).NotTo(Equal(h2))
		})
	})

	// Keep intstr import referenced for the port equality helper above.
	_ = intstr.FromInt(0)
	_ = fmt.Sprintf
	_ = time.Second
})
