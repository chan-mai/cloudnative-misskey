/*
Copyright (C) 2026 chan-mai

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;secrets;persistentvolumeclaims;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;scheduledbackups;poolers,verbs=get;list;watch;create;update;patch;delete

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

// deploymentReady: Deploymentのavailable>=desiredでcondition判定(desired=0はStopped)
func (r *MisskeyReconciler) deploymentReady(ctx context.Context, m *misskeyv1alpha1.Misskey, name string) (metav1.ConditionStatus, string, string) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, dep); err != nil {
		return metav1.ConditionFalse, "NotCreated", "Deployment未作成"
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired == 0 {
		return metav1.ConditionFalse, "Stopped", "replicas=0"
	}
	msg := fmt.Sprintf("%d/%d available", dep.Status.AvailableReplicas, desired)
	if dep.Status.AvailableReplicas >= desired {
		return metav1.ConditionTrue, "Available", msg
	}
	return metav1.ConditionFalse, "Progressing", msg
}

// databaseCondition: managed CNPGのreadyInstances判定、external=True
func (r *MisskeyReconciler) databaseCondition(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) (metav1.ConditionStatus, string, string) {
	if !p.dbManaged {
		return metav1.ConditionTrue, "External", "外部DB"
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: nameDB(m), Namespace: m.Namespace}, cluster); err != nil {
		return metav1.ConditionFalse, "Pending", "CNPG Cluster未作成"
	}
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	desired := int64(int32OrDefault(m.Spec.Postgres.Instances, 1))
	msg := fmt.Sprintf("%d/%d instances ready", ready, desired)
	if desired > 0 && ready >= desired {
		return metav1.ConditionTrue, "Ready", msg
	}
	return metav1.ConditionFalse, "Progressing", msg
}

// migrationCondition: 現行migration Jobの状態
func (r *MisskeyReconciler) migrationCondition(ctx context.Context, m *misskeyv1alpha1.Misskey) (metav1.ConditionStatus, string, string) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: m.Namespace}, job); err != nil {
		return metav1.ConditionFalse, "Pending", "migration Job未作成"
	}
	switch {
	case job.Status.Succeeded >= 1:
		return metav1.ConditionTrue, "Complete", "migration完了"
	case job.Status.Failed >= 1:
		return metav1.ConditionFalse, "Failed", fmt.Sprintf("migration Job失敗 (%d)", job.Status.Failed)
	default:
		return metav1.ConditionFalse, "Running", "migration実行中"
	}
}

// ingressCondition: Ingress存在で判定(外部LBアドレスはcontroller依存なので見ない)
func (r *MisskeyReconciler) ingressCondition(ctx context.Context, m *misskeyv1alpha1.Misskey) (metav1.ConditionStatus, string, string) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, ing); err != nil {
		return metav1.ConditionFalse, "Pending", "Ingress未作成"
	}
	return metav1.ConditionTrue, "Created", "Ingress生成済み"
}

// databaseReady: gate用。databaseConditionがTrueか
func (r *MisskeyReconciler) databaseReady(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) bool {
	st, _, _ := r.databaseCondition(ctx, m, p)
	return st == metav1.ConditionTrue
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
	// 隔離はpod作成前に
	if err := r.reconcileTenancy(ctx, m); err != nil {
		return err
	}
	if p.dbManaged {
		if err := r.reconcilePostgres(ctx, m); err != nil {
			return err
		}
		if err := r.reconcilePoolers(ctx, m); err != nil {
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
	// app Serviceは常時。app/worker DeploymentはDB ready+migration完了までgate
	if err := r.reconcileAppService(ctx, m); err != nil {
		return err
	}
	if r.databaseReady(ctx, m, p) {
		complete, err := r.reconcileMigration(ctx, m, p)
		if err != nil {
			return err
		}
		if complete {
			if err := r.reconcileApp(ctx, m, p); err != nil {
				return err
			}
			if err := r.reconcileWorker(ctx, m, p); err != nil {
				return err
			}
		}
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
	p := resolve(m)
	type cnd struct {
		typ             string
		status          metav1.ConditionStatus
		reason, message string
	}
	var set []cnd
	var remove []string

	dbSt, dbR, dbM := r.databaseCondition(ctx, m, p)
	set = append(set, cnd{"DatabaseReady", dbSt, dbR, dbM})
	mSt, mR, mM := r.migrationCondition(ctx, m)
	set = append(set, cnd{"MigrationComplete", mSt, mR, mM})
	aSt, aR, aM := r.deploymentReady(ctx, m, nameApp(m))
	set = append(set, cnd{"AppReady", aSt, aR, aM})
	wSt, wR, wM := r.deploymentReady(ctx, m, nameWorker(m))
	set = append(set, cnd{"WorkerReady", wSt, wR, wM})
	if boolOr(m.Spec.Proxy.Enabled, true) {
		pSt, pR, pM := r.deploymentReady(ctx, m, nameProxy(m))
		set = append(set, cnd{"ProxyReady", pSt, pR, pM})
	} else {
		remove = append(remove, "ProxyReady")
	}
	if boolOr(m.Spec.Ingress.Enabled, true) {
		iSt, iR, iM := r.ingressCondition(ctx, m)
		set = append(set, cnd{"IngressReady", iSt, iR, iM})
	} else {
		remove = append(remove, "IngressReady")
	}

	ready := true
	for _, c := range set {
		if c.status != metav1.ConditionTrue {
			ready = false
		}
	}

	// 集約Ready
	readySt := metav1.ConditionTrue
	readyReason := "Ready"
	readyMsg := "all subsystems ready"
	phase := "Running"
	switch {
	case reconcileErr != nil:
		ready = false
		readySt = metav1.ConditionFalse
		readyReason = "ReconcileError"
		readyMsg = reconcileErr.Error()
		phase = "Error"
	case ready:
		// Running
	default:
		readySt = metav1.ConditionFalse
		readyReason = "Progressing"
		readyMsg = "waiting for subsystems"
		phase = "Progressing"
	}

	// conflict時はGet-modify-Updateごとやり直す
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &misskeyv1alpha1.Misskey{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(m), cur); err != nil {
			return err
		}
		now := metav1.Now()
		for _, c := range set {
			apimeta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
				Type: c.typ, Status: c.status, Reason: c.reason, Message: c.message,
				ObservedGeneration: cur.Generation, LastTransitionTime: now,
			})
		}
		apimeta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: readySt, Reason: readyReason, Message: readyMsg,
			ObservedGeneration: cur.Generation, LastTransitionTime: now,
		})
		for _, t := range remove {
			apimeta.RemoveStatusCondition(&cur.Status.Conditions, t)
		}
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
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}
