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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// Misskeyオブジェクトをreconcileする
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
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;scheduledbackups,verbs=get;list;watch;create;update;patch;delete

// Misskeyインスタンスを望ましい状態へ収束させる
func (r *MisskeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m misskeyv1alpha1.Misskey
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reconcileErr := r.reconcileAll(ctx, &m)
	ready, statusErr := r.updateStatus(ctx, &m, reconcileErr)
	if statusErr != nil {
		logger.Error(statusErr, "failed to update status")
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if !ready {
		// podが起動途中の可能性(例: CNPGのapp secret出現待ち)
		// 所有イベントが発火しなくてもstatusが収束するようrequeue
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// app Deploymentのavailableレプリカ数からappが提供中かを判定
// pod起動前にstatusがRunningを主張するのを防ぐ
func (r *MisskeyReconciler) assessReady(ctx context.Context, m *misskeyv1alpha1.Misskey) (bool, string, string) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameApp(m), Namespace: m.Namespace}, dep); err != nil {
		return false, "Pending", "app Deployment not created yet"
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	// replicas=0は意図的停止。Ready扱いにしない
	if desired == 0 {
		return false, "Stopped", "app scaled to 0"
	}
	msg := fmt.Sprintf("%d/%d app replicas available", dep.Status.AvailableReplicas, desired)
	if dep.Status.AvailableReplicas >= desired {
		return true, "Available", msg
	}
	return false, "Progressing", msg
}

// 全子リソースを依存順に生成
func (r *MisskeyReconciler) reconcileAll(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	p := resolve(m)

	// planがこれらのsecretを参照するため、pod前に用意
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

// reconcile結果とappの実ヘルスをMisskeyのstatusサブリソースに反映
// インスタンスがReadyかを返す
func (r *MisskeyReconciler) updateStatus(ctx context.Context, m *misskeyv1alpha1.Misskey, reconcileErr error) (bool, error) {
	ready, reason, message := r.assessReady(ctx, m)
	condStatus := metav1.ConditionTrue
	phase := "Running"
	switch {
	case reconcileErr != nil:
		ready = false
		condStatus = metav1.ConditionFalse
		reason = "ReconcileError"
		message = reconcileErr.Error()
		phase = "Error"
	case ready:
		phase = "Running"
	default:
		condStatus = metav1.ConditionFalse
		phase = "Progressing"
	}

	// conflict時はGet-modify-Updateごとやり直す
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &misskeyv1alpha1.Misskey{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(m), cur); err != nil {
			return err
		}
		apimeta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
			LastTransitionTime: metav1.Now(),
		})
		cur.Status.Phase = phase
		cur.Status.ObservedGeneration = cur.Generation
		return r.Status().Update(ctx, cur)
	})
	return ready, client.IgnoreNotFound(err)
}

// コントローラと所有リソースを結線
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
