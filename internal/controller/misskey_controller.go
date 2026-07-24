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
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// Misskeyオブジェクトをreconcileする
type MisskeyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// DriftResyncInterval: ready収束後の定期requeue間隔。watchしない外部CRD
	// (CNPG/redis-operator/KEDA)のSSA再適用でドリフトを是正。0で既定
	DriftResyncInterval time.Duration
	// Digests: trackImageDigest時のtag→digest解決(channel controllerと共有)
	Digests *DigestResolver
}

// watchしない外部CRDのドリフト是正requeueの既定間隔
const defaultDriftResyncInterval = 3 * time.Minute

// driftInterval: ready後の定期requeue間隔(未設定は既定)
func (r *MisskeyReconciler) driftInterval() time.Duration {
	if r.DriftResyncInterval > 0 {
		return r.DriftResyncInterval
	}
	return defaultDriftResyncInterval
}

// truncateMsg: Event/statusメッセージ長の上限。過大なエラー全文によるAPI負荷/可読性劣化を防ぐ
// 接尾辞込みで上限内に収め、UTF-8のマルチバイト境界で切らない
func truncateMsg(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	const suffix = "…(truncated)"
	cut := max - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// event: Recorder配線時のみEventを発行(テスト等の未配線ではno-op)
// actionはregardingに対して行った操作(UpperCamelCase)、noteは人間可読メッセージ
func (r *MisskeyReconciler) event(m *misskeyv1beta1.Misskey, eventType, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(m, nil, eventType, reason, action, note, args...)
}

// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeys/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;persistentvolumeclaims;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// Secretはwatch/Ownsせずname指定getのみ(cluster全Secretのlist/watch権限を持たない)
// operatorはSecretを削除しないためdeleteも付与しない(生成SecretのcleanupはownerRef GCに委ねる)
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeychannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;scheduledbackups;backups;poolers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redis.opstreelabs.in,resources=redisreplications;redissentinels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects;triggerauthentications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;podmonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete

// Misskeyインスタンスを望ましい状態へ収束させる
func (r *MisskeyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var m misskeyv1beta1.Misskey
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 削除中はdeletionPolicyに従いデータ保持してfinalizerを外す
	if !m.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &m)
	}
	// deletionPolicy処理のためfinalizerを付与
	if controllerutil.AddFinalizer(&m, misskeyFinalizer) {
		if err := r.Update(ctx, &m); err != nil {
			return ctrl.Result{}, err
		}
	}

	// imageFrom時はChannelからimageを解決(in-memoryのみ, finalizer付与後なのでpersistされない)
	reconcileErr := r.resolveImage(ctx, &m)
	if reconcileErr == nil {
		reconcileErr = r.reconcileAll(ctx, &m)
	}
	if reconcileErr != nil {
		r.event(&m, corev1.EventTypeWarning, "ReconcileError", "Reconcile", "%s", truncateMsg(reconcileErr.Error()))
	}
	ready, statusErr := r.updateStatus(ctx, &m, reconcileErr)
	if statusErr != nil {
		logger.Error(statusErr, "failed to update status")
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if !ready {
		// suspend中は定常状態のため15sスピンせずdrift間隔で十分
		if m.Spec.Suspend {
			return ctrl.Result{RequeueAfter: r.driftInterval()}, nil
		}
		// podが起動途中の可能性(例: CNPGのapp secret出現待ち)
		// 所有イベントが発火しなくてもstatusが収束するようrequeue
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	// ready後もwatchしない外部CRDのドリフト是正のため定期requeue
	return ctrl.Result{RequeueAfter: r.driftInterval()}, nil
}

// deploymentReady: Deploymentのavailable>=desiredでcondition判定(desired=0はStopped)
func (r *MisskeyReconciler) deploymentReady(ctx context.Context, m *misskeyv1beta1.Misskey, name string) (metav1.ConditionStatus, string, string) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, dep); err != nil {
		return metav1.ConditionFalse, "NotCreated", "Deployment not created"
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
func (r *MisskeyReconciler) databaseCondition(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (metav1.ConditionStatus, string, string) {
	if !p.dbManaged {
		return metav1.ConditionTrue, "External", "external database"
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: nameDB(m), Namespace: m.Namespace}, cluster); err != nil {
		return metav1.ConditionFalse, "Pending", "CNPG Cluster not created"
	}
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	desired := int64(int32OrDefault(m.Spec.Postgres.Instances, 1))
	msg := fmt.Sprintf("%d/%d instances ready", ready, desired)
	if desired > 0 && ready >= desired {
		return metav1.ConditionTrue, "Ready", msg
	}
	return metav1.ConditionFalse, "Progressing", msg
}

// redisCondition: managed redisインスタンスの可用性を集約
// standaloneはSTSのreadyReplicas、HAはOT operator管理podのready数をappラベルで判定
// (既存NP/PodMonitorと同じラベル依存)。managedが無い(全external)場合はTrue
func (r *MisskeyReconciler) redisCondition(ctx context.Context, m *misskeyv1beta1.Misskey) (metav1.ConditionStatus, string, string) {
	instances := managedRedisInstances(m)
	if len(instances) == 0 {
		return metav1.ConditionTrue, "External", "external Redis"
	}
	msgs := make([]string, 0, len(instances))
	allReady := true
	for _, inst := range instances {
		name := nameRedisInstance(m, inst.suffix)
		var ready, desired int32
		if inst.ha {
			desired = inst.replicas
			ready = r.readyPodsByAppLabel(ctx, m.Namespace, nameRedisHA(m, inst.suffix))
		} else {
			desired = 1
			sts := &appsv1.StatefulSet{}
			if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, sts); err == nil {
				ready = sts.Status.ReadyReplicas
			}
		}
		if ready < desired {
			allReady = false
		}
		msgs = append(msgs, fmt.Sprintf("%s %d/%d", name, ready, desired))
	}
	msg := strings.Join(msgs, ", ")
	if allReady {
		return metav1.ConditionTrue, "Ready", msg
	}
	return metav1.ConditionFalse, "Progressing", msg
}

// readyPodsByAppLabel: app=<name>のpodのうちReadyな数
func (r *MisskeyReconciler) readyPodsByAppLabel(ctx context.Context, ns, app string) int32 {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"app": app}); err != nil {
		return 0
	}
	var ready int32
	for i := range pods.Items {
		for _, c := range pods.Items[i].Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready
}

// searchCondition: managed MeiliSearchのSTS可用性。external=True(meilisearch以外はcondition自体を外す)
func (r *MisskeyReconciler) searchCondition(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (metav1.ConditionStatus, string, string) {
	if !p.meiliManaged {
		return metav1.ConditionTrue, "External", "external MeiliSearch"
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameMeili(m), Namespace: m.Namespace}, sts); err != nil {
		return metav1.ConditionFalse, "Pending", "MeiliSearch StatefulSet not created"
	}
	if sts.Status.ReadyReplicas >= 1 {
		return metav1.ConditionTrue, "Ready", "1/1 ready"
	}
	return metav1.ConditionFalse, "Progressing", "0/1 ready"
}

// objectStorageCondition: meta書込Jobの状態。autoConfigure=falseはUnmanaged(True・非gating)
func (r *MisskeyReconciler) objectStorageCondition(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (metav1.ConditionStatus, string, string) {
	if !p.objAutoConfigure {
		return metav1.ConditionTrue, "Unmanaged", "autoConfigure=false; apply object storage settings to the meta table yourself"
	}
	assigns, err := objectStorageAssignments(p)
	if err != nil {
		return metav1.ConditionFalse, "InvalidSpec", err.Error()
	}
	sql := renderObjectStorageSQL(assigns)
	hash := r.objectStorageHash(ctx, m, p, sql, assigns)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameObjectStorage(m, hash), Namespace: m.Namespace}, job); err != nil {
		return metav1.ConditionFalse, "Pending", "meta Job not created"
	}
	switch {
	case job.Status.Succeeded >= 1:
		return metav1.ConditionTrue, "Configured", "object storage written to the meta table"
	case job.Status.Failed >= 1:
		return metav1.ConditionFalse, "Failed", "meta Job failed"
	default:
		return metav1.ConditionFalse, "Progressing", "meta Job running"
	}
}

// migrationCondition: 現行migration Jobの状態
func (r *MisskeyReconciler) migrationCondition(ctx context.Context, m *misskeyv1beta1.Misskey) (metav1.ConditionStatus, string, string) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: m.Namespace}, job); err != nil {
		return metav1.ConditionFalse, "Pending", "migration Job not created"
	}
	switch {
	case job.Status.Succeeded >= 1:
		return metav1.ConditionTrue, "Complete", "migration complete"
	case job.Status.Failed >= 1:
		return metav1.ConditionFalse, "Failed", fmt.Sprintf("migration Job failed (%d)", job.Status.Failed)
	default:
		return metav1.ConditionFalse, "Running", "migration running"
	}
}

// ingressCondition: Ingress存在で判定(外部LBアドレスはcontroller依存なので見ない)
func (r *MisskeyReconciler) ingressCondition(ctx context.Context, m *misskeyv1beta1.Misskey) (metav1.ConditionStatus, string, string) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, ing); err != nil {
		return metav1.ConditionFalse, "Pending", "Ingress not created"
	}
	return metav1.ConditionTrue, "Created", "Ingress created"
}

// databaseReady: gate用。databaseConditionがTrueか
func (r *MisskeyReconciler) databaseReady(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) bool {
	st, _, _ := r.databaseCondition(ctx, m, p)
	return st == metav1.ConditionTrue
}

// redisReady: gate用, redisConditionがTrueか
func (r *MisskeyReconciler) redisReady(ctx context.Context, m *misskeyv1beta1.Misskey) bool {
	st, _, _ := r.redisCondition(ctx, m)
	return st == metav1.ConditionTrue
}

// 全子リソースを依存順に生成
func (r *MisskeyReconciler) reconcileAll(ctx context.Context, m *misskeyv1beta1.Misskey) error {
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
		if err := r.reconcileBackupVerify(ctx, m); err != nil {
			return err
		}
	}
	// 常に呼ぶ(managed provisioning + external化/role削除/mode切替のcleanup)
	if err := r.reconcileRedis(ctx, m); err != nil {
		return err
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
	// objectStorage無効 or autoConfigure=false時はJob/SQL CMを掃除(metaは触らない)
	if !p.objAutoConfigure {
		if err := r.cleanupObjectStorage(ctx, m); err != nil {
			return err
		}
	}
	if m.Spec.Suspend {
		// 休止: app/workerを0に落としautoscaler削除。migration/objectStorage Jobの新規作成もskip
		// 実行中のmigration Jobは走り切らせる(途中killの方が危険)
		if err := r.suspendWorkloads(ctx, m); err != nil {
			return err
		}
	} else if r.databaseReady(ctx, m, p) {
		// preBackup有効時はon-demandバックアップ完了まで新migrationをgate
		backupDone, err := r.reconcilePreMigrationBackup(ctx, m, p)
		if err != nil {
			return err
		}
		complete := false
		if backupDone {
			if complete, err = r.reconcileMigration(ctx, m, p); err != nil {
				return err
			}
		}
		if complete {
			// objectStorage設定をmetaへ投入(autoConfigure時)。書込完了までapp/workerをgate
			objReady := true
			if p.objAutoConfigure {
				objReady, err = r.reconcileObjectStorage(ctx, m, p)
				if err != nil {
					return err
				}
			}
			// redis ready後にのみapp/workerをroll, sentinel準備前のroll回避(pub/sub購読失敗防止)
			if objReady && r.redisReady(ctx, m) {
				if err := r.reconcileApp(ctx, m, p); err != nil {
					return err
				}
				if err := r.reconcileWorker(ctx, m, p); err != nil {
					return err
				}
			}
		}
	}
	// 常に呼ぶ(有効時provisioning + 無効化時cleanup)
	if err := r.reconcileProxy(ctx, m); err != nil {
		return err
	}
	if err := r.reconcileIngress(ctx, m, p); err != nil {
		return err
	}
	if err := r.reconcileMonitoring(ctx, m, p); err != nil {
		return err
	}
	return nil
}

// reconcile結果とappの実ヘルスをMisskeyのstatusサブリソースに反映
// インスタンスがReadyかを返す
func (r *MisskeyReconciler) updateStatus(ctx context.Context, m *misskeyv1beta1.Misskey, reconcileErr error) (bool, error) {
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
	rdSt, rdR, rdM := r.redisCondition(ctx, m)
	set = append(set, cnd{"RedisReady", rdSt, rdR, rdM})
	if p.meiliEnabled {
		seSt, seR, seM := r.searchCondition(ctx, m, p)
		set = append(set, cnd{"SearchReady", seSt, seR, seM})
	} else {
		remove = append(remove, "SearchReady")
	}
	mSt, mR, mM := r.migrationCondition(ctx, m)
	set = append(set, cnd{"MigrationComplete", mSt, mR, mM})
	if p.objEnabled {
		oSt, oR, oM := r.objectStorageCondition(ctx, m, p)
		set = append(set, cnd{"ObjectStorageConfigured", oSt, oR, oM})
	} else {
		remove = append(remove, "ObjectStorageConfigured")
	}
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
		readyMsg = truncateMsg(reconcileErr.Error())
		phase = "Error"
	case m.Spec.Suspend:
		ready = false
		readySt = metav1.ConditionFalse
		readyReason = "Suspended"
		readyMsg = "suspended by spec.suspend"
		phase = "Suspended"
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
		cur := &misskeyv1beta1.Misskey{}
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
		// 解決済みの接続先を運用向けに公開(pooler/rw/external, sentinel service, index)
		cur.Status.DatabaseHost = p.dbHost
		cur.Status.RedisHost = p.redisDefault.host
		if p.meiliEnabled {
			cur.Status.SearchIndex = p.meiliIndex
		} else {
			cur.Status.SearchIndex = ""
		}
		cur.Status.Backup = r.readBackupStatus(ctx, m, p)
		// 解決済みimage(imageFrom時はchannel解決値)。channel controllerの追従集計が参照
		cur.Status.Image = m.Spec.Image
		return r.Status().Update(ctx, cur)
	})
	return ready, client.IgnoreNotFound(err)
}

// readBackupStatus: CNPG Cluster statusのバックアップ時刻をstatusへ写す(backup有効時のみ)
func (r *MisskeyReconciler) readBackupStatus(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) *misskeyv1beta1.BackupStatus {
	if !p.dbManaged || m.Spec.Postgres.Backup == nil {
		return nil
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: nameDB(m), Namespace: m.Namespace}, cluster); err != nil {
		return nil
	}
	st := &misskeyv1beta1.BackupStatus{}
	if s, _, _ := unstructured.NestedString(cluster.Object, "status", "lastSuccessfulBackup"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			st.LastSuccessfulBackup = metav1.NewTime(t)
		}
	}
	if s, _, _ := unstructured.NestedString(cluster.Object, "status", "firstRecoverabilityPoint"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			st.FirstRecoverabilityPoint = metav1.NewTime(t)
		}
	}
	if st.LastSuccessfulBackup.IsZero() && st.FirstRecoverabilityPoint.IsZero() {
		return nil
	}
	return st
}

// misskeysForChannel: Channel変更を参照する全namespaceのMisskeyへ広播(image解決の再評価)
func (r *MisskeyReconciler) misskeysForChannel(ctx context.Context, obj client.Object) []reconcile.Request {
	var list misskeyv1beta1.MisskeyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.ImageFrom != nil && m.Spec.ImageFrom.Channel == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		}
	}
	return reqs
}

// コントローラと所有リソースを結線
func (r *MisskeyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Secretはwatch/Ownsせず(cluster-wide list/watchを排し、cacheにも全Secretを載せない)
	// managed Secretの変更検知とローテはconfig-checksum + drift resyncで代替する
	return ctrl.NewControllerManagedBy(mgr).
		For(&misskeyv1beta1.Misskey{}).
		Watches(&misskeyv1beta1.MisskeyChannel{}, handler.EnqueueRequestsFromMapFunc(r.misskeysForChannel)).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&corev1.LimitRange{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Complete(r)
}
