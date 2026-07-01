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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// MisskeyReconciler reconciles a Misskey object.
type MisskeyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;secrets;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;scheduledbackups,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the Misskey instance toward its desired state.
func (r *MisskeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m misskeyv1alpha1.Misskey
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reconcileErr := r.reconcileAll(ctx, &m)
	if statusErr := r.updateStatus(ctx, &m, reconcileErr); statusErr != nil {
		logger.Error(statusErr, "failed to update status")
	}
	return ctrl.Result{}, reconcileErr
}

// reconcileAll materializes every child resource in dependency order.
func (r *MisskeyReconciler) reconcileAll(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	p := resolve(m)

	// The plan references these secrets, so ensure them before the pods.
	if p.meiliManaged {
		if err := r.reconcileMeiliSecret(ctx, m); err != nil {
			return err
		}
	}
	if p.setupManaged {
		if err := r.reconcileSetupSecret(ctx, m); err != nil {
			return err
		}
	}
	if err := r.reconcileConfigMaps(ctx, m, p); err != nil {
		return err
	}
	if p.dbManaged {
		if err := r.reconcilePostgres(ctx, m); err != nil {
			return err
		}
	}
	if p.redisManaged {
		if err := r.reconcileRedis(ctx, m); err != nil {
			return err
		}
	}
	if p.meiliManaged {
		if err := r.reconcileMeilisearch(ctx, m, p); err != nil {
			return err
		}
	}
	if err := r.reconcileApp(ctx, m, p); err != nil {
		return err
	}
	if err := r.reconcileWorker(ctx, m, p); err != nil {
		return err
	}
	if boolOr(m.Spec.Proxy.Enabled, true) {
		if err := r.reconcileProxy(ctx, m); err != nil {
			return err
		}
	}
	if boolOr(m.Spec.Ingress.Enabled, true) {
		if err := r.reconcileIngress(ctx, m, p); err != nil {
			return err
		}
	}
	return nil
}

// updateStatus reflects the reconcile outcome on the Misskey status subresource.
func (r *MisskeyReconciler) updateStatus(ctx context.Context, m *misskeyv1alpha1.Misskey, reconcileErr error) error {
	cur := &misskeyv1alpha1.Misskey{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(m), cur); err != nil {
		return client.IgnoreNotFound(err)
	}

	cond := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: cur.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if reconcileErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ReconcileError"
		cond.Message = reconcileErr.Error()
		cur.Status.Phase = "Error"
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Reconciled"
		cond.Message = "All components reconciled"
		cur.Status.Phase = "Running"
	}
	apimeta.SetStatusCondition(&cur.Status.Conditions, cond)
	cur.Status.ObservedGeneration = cur.Generation
	return r.Status().Update(ctx, cur)
}

// SetupWithManager wires the controller and the resources it owns.
func (r *MisskeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&misskeyv1alpha1.Misskey{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}
