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
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ubiv1alpha1 "github.com/yfxue/pi-agent-operator/api/v1alpha1"
)

// PiWorkerReconciler reconciles a PiWorker object into a hardened Pi coding-agent workload
// exposed through an authenticated WebSocket endpoint behind a per-instance Ingress.
type PiWorkerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
}

// +kubebuilder:rbac:groups=ubi.pi.dev,resources=piworkers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ubi.pi.dev,resources=piworkers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ubi.pi.dev,resources=piworkers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the cluster toward the desired state for a single PiWorker: it generates
// a stable random host, reconciles an owned Deployment, Service, and Ingress, and reports
// status conditions derived from the child resources.
func (r *PiWorkerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	piworker := &ubiv1alpha1.PiWorker{}
	if err := r.Get(ctx, req.NamespacedName, piworker); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	n := resourceNames(piworker)

	// Reconcile the auto-generated client bearer token Secret FIRST. The CRD marks
	// status.bearerTokenSecretRef as required (minLength 1), so any Status().Update
	// that carries an empty bearerTokenSecretRef is rejected by the API server and
	// the reconciler loops forever. Generating the token secret before writing
	// status guarantees bearerTokenSecretRef is populated on the first status write.
	if err := r.reconcileTokenSecret(ctx, piworker, n); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile the LiteLLM API-key Secret from spec.litellm.apiKey. The key value is
	// provided by the client in the CR; the controller projects it into the worker Pod
	// from this controller-owned Secret, which is garbage-collected with the PiWorker.
	if err := r.reconcileLiteLLMSecret(ctx, piworker, n); err != nil {
		return ctrl.Result{}, err
	}

	// Generate the random host once and persist it before building the Ingress so it is
	// stable across reconciles.
	if piworker.Status.Host == "" {
		host, err := generateHost(r.Config.IngressDomain)
		if err != nil {
			logger.Error(err, "failed to generate host")
			return ctrl.Result{}, err
		}
		piworker.Status.Host = host
		piworker.Status.Endpoint = endpointScheme(piworker, r.Config.TLSSecretName) + "://" + host + rpcPath
		piworker.Status.WorkloadName = n.deployment
		piworker.Status.ServiceName = n.service
		piworker.Status.IngressName = n.ingress
		if err := r.Status().Update(ctx, piworker); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Ensure status-derived names are populated for older resources.
	piworker.Status.WorkloadName = n.deployment
	piworker.Status.ServiceName = n.service
	piworker.Status.IngressName = n.ingress
	piworker.Status.Endpoint = endpointScheme(piworker, r.Config.TLSSecretName) + "://" + piworker.Status.Host + rpcPath

	var requeue error

	if err := r.reconcileDeployment(ctx, piworker, n); err != nil {
		requeue = err
	}
	if err := r.reconcileService(ctx, piworker, n); err != nil && requeue == nil {
		requeue = err
	}
	if err := r.reconcileIngress(ctx, piworker, n); err != nil && requeue == nil {
		requeue = err
	}

	// Compute and persist status conditions.
	if err := r.updateStatus(ctx, piworker, requeue); err != nil {
		logger.Error(err, "failed to update status")
		if requeue == nil {
			requeue = err
		}
	}

	if requeue != nil {
		logger.V(1).Info("reconcile completed with requeue", "err", requeue)
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

func (r *PiWorkerReconciler) reconcileDeployment(ctx context.Context, piworker *ubiv1alpha1.PiWorker, n names) error {
	dep := r.desiredDeployment(piworker, n)
	if err := controllerutil.SetControllerReference(piworker, dep, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, dep, &appsv1.Deployment{})
}

func (r *PiWorkerReconciler) reconcileService(ctx context.Context, piworker *ubiv1alpha1.PiWorker, n names) error {
	svc := r.desiredService(piworker, n)
	if err := controllerutil.SetControllerReference(piworker, svc, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, svc, &corev1.Service{})
}

func (r *PiWorkerReconciler) reconcileIngress(ctx context.Context, piworker *ubiv1alpha1.PiWorker, n names) error {
	ing := r.desiredIngress(piworker, n, piworker.Status.Host)
	if err := controllerutil.SetControllerReference(piworker, ing, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, ing, &networkingv1.Ingress{})
}

// reconcileTokenSecret ensures a controller-owned Secret holding the auto-generated client
// bearer token exists. On first reconcile it generates a token, creates the Secret, sets an
// owner reference, and records the Secret name/key in status so kubectl can retrieve the
// token. On later reconciles the existing Secret data is preserved — the token is generated
// once per PiWorker and never rotated or overwritten by the controller. The token value is
// never logged or placed in status.
func (r *PiWorkerReconciler) reconcileTokenSecret(ctx context.Context, piworker *ubiv1alpha1.PiWorker, n names) error {
	// Already reconciled: just ensure the Secret still exists.
	if piworker.Status.BearerTokenSecretRef.Name != "" {
		existing := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: piworker.Namespace, Name: piworker.Status.BearerTokenSecretRef.Name}, existing); err != nil {
			if apierrors.IsNotFound(err) {
				// Fall through to recreate with a fresh token.
			} else {
				return err
			}
		} else {
			return nil
		}
	}

	token, err := generateBearerToken()
	if err != nil {
		return err
	}
	sec := r.desiredTokenSecret(piworker, n, token)
	if err := controllerutil.SetControllerReference(piworker, sec, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	piworker.Status.BearerTokenSecretRef = ubiv1alpha1.SecretKeySelector{
		Name: n.tokenSecret,
		Key:  bearerTokenKey,
	}
	return r.Status().Update(ctx, piworker)
}

// reconcileLiteLLMSecret ensures a controller-owned Secret holding the LiteLLM API key
// exists. The key value comes from spec.litellm.apiKey (provided by the client at CR
// creation); the controller projects it into the worker Pod from this Secret, which is
// garbage-collected with the PiWorker via its owner reference.
func (r *PiWorkerReconciler) reconcileLiteLLMSecret(ctx context.Context, piworker *ubiv1alpha1.PiWorker, n names) error {
	sec := r.desiredLiteLLMSecret(piworker, n)
	if err := controllerutil.SetControllerReference(piworker, sec, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdate(ctx, sec, &corev1.Secret{})
}

// createOrUpdate creates the object if absent or updates mutable fields to match the desired
// object. It relies on controller-runtime's semantic equality via the provided zero-valued
// existing object type for server-side apply-free mutation.
func (r *PiWorkerReconciler) createOrUpdate(ctx context.Context, desired client.Object, existing client.Object) error {
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Preserve the runtime object's GVK and metadata uid/resourceVersion; patch the spec.
	patchBase := existing.DeepCopyObject().(client.Object)
	// Ensure the desired object carries its GVK so controller-runtime can build the patch
	// URL; objects constructed by the builders may lack APIVersion/Kind until the scheme
	// injects them, and a missing GVK surfaces as "name must be provided" patch errors.
	if gvk := existing.GetObjectKind().GroupVersionKind(); gvk.Kind != "" {
		desired.GetObjectKind().SetGroupVersionKind(gvk)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	desired.SetUID(existing.GetUID())
	desired.SetGeneration(existing.GetGeneration())
	// Merge labels and annotations.
	mergedLabels := existing.GetLabels()
	if mergedLabels == nil {
		mergedLabels = map[string]string{}
	}
	for k, v := range desired.GetLabels() {
		mergedLabels[k] = v
	}
	desired.SetLabels(mergedLabels)
	if !specEqual(existing, desired) {
		if err := r.Patch(ctx, desired, client.MergeFrom(patchBase)); err != nil {
			return fmt.Errorf("patch %s/%s: %w", desired.GetName(), desired.GetNamespace(), err)
		}
	}
	return nil
}

// specEqual reports whether the controller-managed spec of a and b match, so the
// reconciler can skip patching when there is no drift. This is required because an empty
// MergeFrom PATCH still bumps the object's generation, which (since the controller Owns
// the resource) re-triggers a reconcile and loops ~10×/sec, racing the Deployment
// controller and stalling status.observedGeneration. The desired objects set all
// API-server-defaulted fields explicitly (see resources.go), so a plain DeepEqual of
// the spec converges; labels are compared separately since createOrUpdate merges them.
func specEqual(a, b client.Object) bool {
	if !reflect.DeepEqual(a.GetLabels(), b.GetLabels()) {
		return false
	}
	switch ca := a.(type) {
	case *appsv1.Deployment:
		cb, ok := b.(*appsv1.Deployment)
		return ok && reflect.DeepEqual(ca.Spec, cb.Spec)
	case *corev1.Service:
		cb, ok := b.(*corev1.Service)
		return ok && reflect.DeepEqual(ca.Spec, cb.Spec)
	case *networkingv1.Ingress:
		cb, ok := b.(*networkingv1.Ingress)
		return ok && reflect.DeepEqual(ca.Spec, cb.Spec)
	case *corev1.Secret:
		cb, ok := b.(*corev1.Secret)
		return ok && reflect.DeepEqual(ca.StringData, cb.StringData) && reflect.DeepEqual(ca.Data, cb.Data)
	default:
		return false
	}
}

// updateStatus derives conditions from the owned Deployment and Ingress and writes status.
func (r *PiWorkerReconciler) updateStatus(ctx context.Context, piworker *ubiv1alpha1.PiWorker, requeue error) error {
	dep := &appsv1.Deployment{}
	getErr := r.Get(ctx, client.ObjectKey{Namespace: piworker.Namespace, Name: piworker.Status.WorkloadName}, dep)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}

	var available bool
	if getErr == nil {
		for _, c := range dep.Status.Conditions {
			if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
				available = true
			}
		}
	}

	setCondition(piworker, "Available", available)
	setCondition(piworker, "Ready", available && requeue == nil)
	if requeue != nil {
		setCondition(piworker, "Degraded", true)
	} else {
		setCondition(piworker, "Degraded", false)
	}
	setCondition(piworker, "Reconciling", requeue != nil)

	piworker.Status.ObservedGeneration = piworker.Generation
	return r.Status().Update(ctx, piworker)
}

// SetupWithManager sets up the controller with the Manager, watching owned child resources.
func (r *PiWorkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ubiv1alpha1.PiWorker{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&corev1.Secret{}).
		Named("piworker").
		Complete(r)
}
