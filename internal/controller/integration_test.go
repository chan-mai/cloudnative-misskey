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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
)

// setupEnvtest: envtest(etcd+apiserver)を起動しclientを返す。KUBEBUILDER_ASSETS未設定ならskip
func setupEnvtest(t *testing.T) (context.Context, client.Client, *runtime.Scheme) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS未設定のためenvtestをskip(make envtestで用意)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := misskeyv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return context.Background(), cl, sch
}

func exists(ctx context.Context, cl client.Client, obj client.Object, name, ns string) bool {
	return cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj) == nil
}

func hasCondition(m *misskeyv1alpha1.Misskey, typ string, want metav1.ConditionStatus) bool {
	for _, c := range m.Status.Conditions {
		if c.Type == typ {
			return c.Status == want
		}
	}
	return false
}

// TestReconcileIntegration: 外部backend構成でreconcileループ全体を検証
// (CNPG/redis-operator CRD不要。migration gate・status・finalizer・削除を通す)
func TestReconcileIntegration(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "itest"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "ex", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:                "https://it.example.com/",
			Image:              "misskey/misskey:x",
			IDGenerationMethod: "aidx",
			SetupPassword:      &misskeyv1alpha1.SetupPasswordSpec{},
			Search:             misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis:   misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
			Ingress: misskeyv1alpha1.IngressSpec{Host: "it.example.com"},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ex", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		reconcile()
	}

	// finalizer付与
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(cur, misskeyFinalizer) {
		t.Error("finalizerが付与されていない")
	}

	// 生成物: config / app Service / migration Job / 隔離NP
	if !exists(ctx, cl, &corev1.ConfigMap{}, nameConfig(m), ns) {
		t.Error("config ConfigMap未生成")
	}
	if !exists(ctx, cl, &corev1.Service{}, nameApp(m), ns) {
		t.Error("app Service未生成")
	}
	if !exists(ctx, cl, &batchv1.Job{}, nameMigrate(m), ns) {
		t.Error("migration Job未生成")
	}

	// migration未完了→app Deployment未生成(gate)
	if exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) {
		t.Error("migration未完了なのにapp Deploymentが生成された")
	}

	// status: external DBはDatabaseReady=True、MigrationComplete=False
	if !hasCondition(cur, "DatabaseReady", metav1.ConditionTrue) {
		t.Errorf("DatabaseReady!=True: %+v", cur.Status.Conditions)
	}
	if !hasCondition(cur, "MigrationComplete", metav1.ConditionFalse) {
		t.Errorf("MigrationComplete!=False: %+v", cur.Status.Conditions)
	}
	// external redisはRedisReady=True、sqlLikeはSearchReadyなし
	if !hasCondition(cur, "RedisReady", metav1.ConditionTrue) {
		t.Errorf("RedisReady!=True (external): %+v", cur.Status.Conditions)
	}
	for _, c := range cur.Status.Conditions {
		if c.Type == "SearchReady" {
			t.Errorf("sqlLikeでSearchReadyが存在: %+v", c)
		}
	}

	// status: 解決済み接続先(external host/redis, indexはsqlLikeで空)
	if cur.Status.DatabaseHost != "pg" {
		t.Errorf("status.databaseHost=%q, want pg", cur.Status.DatabaseHost)
	}
	if cur.Status.RedisHost != "redis" {
		t.Errorf("status.redisHost=%q, want redis", cur.Status.RedisHost)
	}
	if cur.Status.SearchIndex != "" {
		t.Errorf("status.searchIndex=%q, want empty (sqlLike)", cur.Status.SearchIndex)
	}

	// migration Jobを成功させ再reconcile→app/worker Deployment生成
	job := &batchv1.Job{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	if err := cl.Status().Update(ctx, job); err != nil {
		t.Fatalf("job status update: %v", err)
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) {
		t.Error("migration完了後もapp Deployment未生成")
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameWorker(m), ns) {
		t.Error("migration完了後もworker Deployment未生成")
	}

	// 削除→finalizer処理→消滅
	if err := cl.Delete(ctx, cur); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	if err := cl.Get(ctx, req.NamespacedName, &misskeyv1alpha1.Misskey{}); !apierrors.IsNotFound(err) {
		t.Errorf("削除後もMisskeyが残存: %v", err)
	}
}

// TestSuspendResume: spec.suspendでapp/workerが0になり、resumeでautoscaling込みで復帰すること
func TestSuspendResume(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "suspend"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "sus", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://sus.example.com/",
			Image:  "misskey/misskey:v1",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			App: misskeyv1alpha1.ComponentSpec{
				Autoscaling: &misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 3},
			},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "sus", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	update := func(mutate func(*misskeyv1alpha1.Misskey)) {
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
			t.Fatal(err)
		}
		mutate(cur)
		if err := cl.Update(ctx, cur); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	succeedMigration := func() {
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
			t.Fatal(err)
		}
		job := &batchv1.Job{}
		if err := cl.Get(ctx, types.NamespacedName{Name: nameMigrate(cur), Namespace: ns}, job); err != nil {
			t.Fatalf("migration job: %v", err)
		}
		job.Status.Succeeded = 1
		if err := cl.Status().Update(ctx, job); err != nil {
			t.Fatalf("job status: %v", err)
		}
	}
	appReplicas := func() int32 {
		dep := &appsv1.Deployment{}
		if err := cl.Get(ctx, types.NamespacedName{Name: nameApp(m), Namespace: ns}, dep); err != nil {
			t.Fatalf("app deployment: %v", err)
		}
		if dep.Spec.Replicas == nil {
			return -1
		}
		return *dep.Spec.Replicas
	}

	// 稼働到達: migration成功→app/worker生成、appはHPA付き
	for i := 0; i < 2; i++ {
		reconcile()
	}
	succeedMigration()
	for i := 0; i < 2; i++ {
		reconcile()
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) || !exists(ctx, cl, &appsv1.Deployment{}, nameWorker(m), ns) {
		t.Fatal("app/worker Deployment未生成")
	}
	if !exists(ctx, cl, &autoscalingv2.HorizontalPodAutoscaler{}, nameApp(m), ns) {
		t.Fatal("app HPA未生成")
	}

	// suspend → replicas 0・HPA削除・Phase=Suspended
	update(func(c *misskeyv1alpha1.Misskey) { c.Spec.Suspend = true })
	reconcile()
	if got := appReplicas(); got != 0 {
		t.Errorf("suspend後のapp replicas=%d, want 0", got)
	}
	if exists(ctx, cl, &autoscalingv2.HorizontalPodAutoscaler{}, nameApp(m), ns) {
		t.Error("suspend後もHPAが残存")
	}
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.Phase != "Suspended" {
		t.Errorf("phase=%q, want Suspended", cur.Status.Phase)
	}
	if !hasCondition(cur, "Ready", metav1.ConditionFalse) {
		t.Errorf("Ready!=False: %+v", cur.Status.Conditions)
	}

	// suspend中のimage変更では新migration Jobを作らない
	update(func(c *misskeyv1alpha1.Misskey) { c.Spec.Image = "misskey/misskey:v2" })
	reconcile()
	cl.Get(ctx, req.NamespacedName, cur)
	if exists(ctx, cl, &batchv1.Job{}, nameMigrate(cur), ns) {
		t.Error("suspend中に新migration Jobが生成された")
	}

	// resume → 新migration完了後にapp replicasがminReplicas(既定1)で再点火
	update(func(c *misskeyv1alpha1.Misskey) { c.Spec.Suspend = false })
	reconcile()
	succeedMigration()
	for i := 0; i < 2; i++ {
		reconcile()
	}
	if got := appReplicas(); got != 1 {
		t.Errorf("resume後のapp replicas=%d, want 1", got)
	}
	if !exists(ctx, cl, &autoscalingv2.HorizontalPodAutoscaler{}, nameApp(m), ns) {
		t.Error("resume後もHPA未生成")
	}
}

// TestChannelResolveNoPersist: imageFromの解決値がworkloadに使われ、APIのspec.imageは空のままなこと
func TestChannelResolveNoPersist(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "chan"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	ch := &misskeyv1alpha1.MisskeyChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Spec:       misskeyv1alpha1.MisskeyChannelSpec{Image: "misskey/misskey:v1"},
	}
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	cr := &MisskeyChannelReconciler{Client: cl, Scheme: sch}
	chReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "stable"}}
	if _, err := cr.Reconcile(ctx, chReq); err != nil {
		t.Fatalf("channel reconcile: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "flt", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:       "https://flt.example.com/",
			ImageFrom: &misskeyv1alpha1.ImageFromSource{Channel: "stable"},
			Search:    misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "flt", Namespace: ns}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	// APIのspec.imageは空のまま(in-memory解決のみ)
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	if cur.Spec.Image != "" {
		t.Errorf("spec.imageがpersistされた: %q", cur.Spec.Image)
	}
	// 解決値はstatusとmigration Job名に現れる
	if cur.Status.Image != "misskey/misskey:v1" {
		t.Errorf("status.image=%q, want misskey/misskey:v1", cur.Status.Image)
	}
	resolved := cur.DeepCopy()
	resolved.Spec.Image = "misskey/misskey:v1"
	if !exists(ctx, cl, &batchv1.Job{}, nameMigrate(resolved), ns) {
		t.Error("解決済みimageのmigration Job未生成")
	}
	// channel側の集計
	if _, err := cr.Reconcile(ctx, chReq); err != nil {
		t.Fatalf("channel reconcile: %v", err)
	}
	curCh := &misskeyv1alpha1.MisskeyChannel{}
	if err := cl.Get(ctx, chReq.NamespacedName, curCh); err != nil {
		t.Fatal(err)
	}
	if curCh.Status.Instances != 1 || curCh.Status.UpdatedInstances != 1 {
		t.Errorf("channel集計: %+v", curCh.Status)
	}
}

// TestChannelStagedRollout: batchPercent=50でbucketが50を跨ぐ2インスタンスの片方だけ切替わること
func TestChannelStagedRollout(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "chan-roll"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	// bucketが50を跨ぐ2つの名前を選ぶ
	var early, late string
	for i := 0; i < 100 && (early == "" || late == ""); i++ {
		name := fmt.Sprintf("inst-%d", i)
		if channelBucket(ns, name) < 50 {
			if early == "" {
				early = name
			}
		} else if late == "" {
			late = name
		}
	}
	if early == "" || late == "" {
		t.Fatal("bucketが50を跨ぐ名前を発見できず")
	}

	ch := &misskeyv1alpha1.MisskeyChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "canary"},
		Spec: misskeyv1alpha1.MisskeyChannelSpec{
			Image:   "misskey/misskey:v1",
			Rollout: &misskeyv1alpha1.ChannelRollout{BatchPercent: 50, Interval: metav1.Duration{Duration: time.Hour}},
		},
	}
	if err := cl.Create(ctx, ch); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	cr := &MisskeyChannelReconciler{Client: cl, Scheme: sch}
	chReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "canary"}}
	if _, err := cr.Reconcile(ctx, chReq); err != nil {
		t.Fatalf("channel reconcile: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	newMK := func(name string) ctrl.Request {
		m := &misskeyv1alpha1.Misskey{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: misskeyv1alpha1.MisskeySpec{
				URL:       fmt.Sprintf("https://%s.example.com/", name),
				ImageFrom: &misskeyv1alpha1.ImageFromSource{Channel: "canary"},
				Search:    misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
				Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
					Host: "pg", Database: "d", User: "u",
					PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
				}},
				Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
			},
		}
		if err := cl.Create(ctx, m); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	}
	reqEarly, reqLate := newMK(early), newMK(late)
	statusImage := func(req ctrl.Request) string {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %s: %v", req.Name, err)
		}
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
			t.Fatal(err)
		}
		return cur.Status.Image
	}

	// 初期状態: 両方v1
	if a, b := statusImage(reqEarly), statusImage(reqLate); a != "misskey/misskey:v1" || b != "misskey/misskey:v1" {
		t.Fatalf("初期image: %s / %s", a, b)
	}

	// image更新→ロールアウト開始。第1バッチ(bucket<50)のみv2
	curCh := &misskeyv1alpha1.MisskeyChannel{}
	if err := cl.Get(ctx, chReq.NamespacedName, curCh); err != nil {
		t.Fatal(err)
	}
	curCh.Spec.Image = "misskey/misskey:v2"
	if err := cl.Update(ctx, curCh); err != nil {
		t.Fatalf("update channel: %v", err)
	}
	if _, err := cr.Reconcile(ctx, chReq); err != nil {
		t.Fatalf("channel reconcile: %v", err)
	}
	if got := statusImage(reqEarly); got != "misskey/misskey:v2" {
		t.Errorf("early(bucket<50)=%s, want v2", got)
	}
	if got := statusImage(reqLate); got != "misskey/misskey:v1" {
		t.Errorf("late(bucket>=50)=%s, want v1", got)
	}
}

// TestOptOutCleanup: proxy/maintenance/ingressの無効化で生成済みリソースが掃除されること
func TestOptOutCleanup(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "optout"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "oo", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://oo.example.com/",
			Image:  "misskey/misskey:x",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	// 統合前構成の残骸を模擬(reconcileで掃除されること=アップグレード互換の回帰確認)
	legacyDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy-maintenance"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy-maintenance"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "caddy", Image: "caddy:2"}}},
			},
		},
	}
	if err := cl.Create(ctx, legacyDep); err != nil {
		t.Fatalf("create legacy maintenance deployment: %v", err)
	}
	legacySvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: ns},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	if err := cl.Create(ctx, legacySvc); err != nil {
		t.Fatalf("create legacy maintenance service: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "oo", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	update := func(mutate func(*misskeyv1alpha1.Misskey)) {
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
			t.Fatal(err)
		}
		mutate(cur)
		if err := cl.Update(ctx, cur); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}

	// 既定(proxy/maintenance/ingress有効)の生成物
	for name, obj := range map[string]client.Object{
		"proxy Deployment": &appsv1.Deployment{},
		"proxy Service":    &corev1.Service{},
		"proxy PDB":        &policyv1.PodDisruptionBudget{},
	} {
		if !exists(ctx, cl, obj, nameProxy(m), ns) {
			t.Errorf("%s未生成", name)
		}
	}
	// 統合後はmaintenance Deployment/Serviceを作らず、legacy残骸も掃除される
	if exists(ctx, cl, &appsv1.Deployment{}, nameMaintenance(m), ns) {
		t.Error("legacy maintenance Deploymentが残存")
	}
	if exists(ctx, cl, &corev1.Service{}, nameMaintenance(m), ns) {
		t.Error("legacy maintenance Serviceが残存")
	}
	if !exists(ctx, cl, &corev1.ConfigMap{}, nameMaintenanceHTML(m), ns) {
		t.Error("maintenance HTML ConfigMap未生成")
	}
	if !exists(ctx, cl, &networkingv1.Ingress{}, m.Name, ns) {
		t.Error("Ingress未生成")
	}

	// maintenanceのみ無効化 → HTML ConfigMapだけ掃除、proxyは残る
	update(func(c *misskeyv1alpha1.Misskey) { c.Spec.Proxy.Maintenance.Enabled = boolPtr(false) })
	reconcile()
	if exists(ctx, cl, &corev1.ConfigMap{}, nameMaintenanceHTML(m), ns) {
		t.Error("maintenance無効化後もHTML ConfigMapが残存")
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameProxy(m), ns) {
		t.Error("maintenance無効化でproxy Deploymentまで消えた")
	}

	// proxy+ingress無効化 → 全掃除
	update(func(c *misskeyv1alpha1.Misskey) {
		c.Spec.Proxy.Enabled = boolPtr(false)
		c.Spec.Ingress.Enabled = boolPtr(false)
	})
	reconcile()
	if exists(ctx, cl, &appsv1.Deployment{}, nameProxy(m), ns) {
		t.Error("proxy無効化後もDeploymentが残存")
	}
	if exists(ctx, cl, &corev1.Service{}, nameProxy(m), ns) {
		t.Error("proxy無効化後もServiceが残存")
	}
	if exists(ctx, cl, &policyv1.PodDisruptionBudget{}, nameProxy(m), ns) {
		t.Error("proxy無効化後もPDBが残存")
	}
	if exists(ctx, cl, &networkingv1.Ingress{}, m.Name, ns) {
		t.Error("ingress無効化後もIngressが残存")
	}
}

// TestMigrationRetryOnSpecChange: 失敗したmigration Jobが入力checksum変化時のみ再生成されること
func TestMigrationRetryOnSpecChange(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "migretry"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "mg", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://mg.example.com/",
			Image:  "misskey/misskey:x",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "mg", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	jobKey := types.NamespacedName{Name: nameMigrate(m), Namespace: ns}
	for i := 0; i < 2; i++ {
		reconcile()
	}

	// Jobを失敗させる
	job := &batchv1.Job{}
	if err := cl.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("get job: %v", err)
	}
	origUID := job.UID
	job.Status.Failed = 1
	if err := cl.Status().Update(ctx, job); err != nil {
		t.Fatalf("job status update: %v", err)
	}

	// 入力不変 → 失敗Jobは保持(手動削除で再試行の設計)
	reconcile()
	if err := cl.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("同一入力の失敗Jobが消された: %v", err)
	}
	if job.UID != origUID {
		t.Error("同一入力なのにJobが作り直された")
	}

	// 入力変更(concurrently flag) → 失敗Jobを削除し再生成
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Migration.CreateIndexConcurrently = boolPtr(true)
	if err := cl.Update(ctx, cur); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcile() // 削除
	reconcile() // 再生成
	if err := cl.Get(ctx, jobKey, job); err != nil {
		t.Fatalf("checksum変化後にJobが再生成されていない: %v", err)
	}
	if job.UID == origUID {
		t.Error("checksum変化後も古い失敗Jobのまま")
	}
	if job.Status.Failed != 0 {
		t.Errorf("再生成Jobのstatusが引き継がれている: %+v", job.Status)
	}
}

// TestSecretRotationRollsPods: 参照Secretの値更新でapp podテンプレートのchecksumが変わること
func TestSecretRotationRollsPods(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "rotate"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgsec", Namespace: ns},
		StringData: map[string]string{"pw": "old"},
	}
	if err := cl.Create(ctx, sec); err != nil {
		t.Fatalf("secret: %v", err)
	}

	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "rot", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://rot.example.com/",
			Image:  "misskey/misskey:x",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "rot", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}

	// migrationを成功させapp Deploymentを生成
	job := &batchv1.Job{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	if err := cl.Status().Update(ctx, job); err != nil {
		t.Fatalf("job status update: %v", err)
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	dep := &appsv1.Deployment{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameApp(m), Namespace: ns}, dep); err != nil {
		t.Fatalf("app deployment: %v", err)
	}
	before := dep.Spec.Template.Annotations[configChecksumAnnotation]
	if before == "" {
		t.Fatal("checksum annotationが空")
	}

	// Secret値をローテーション → checksumが変わりpodがローリングする
	if err := cl.Get(ctx, types.NamespacedName{Name: "pgsec", Namespace: ns}, sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["pw"] = []byte("new")
	if err := cl.Update(ctx, sec); err != nil {
		t.Fatalf("secret update: %v", err)
	}
	reconcile()
	if err := cl.Get(ctx, types.NamespacedName{Name: nameApp(m), Namespace: ns}, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations[configChecksumAnnotation] == before {
		t.Error("Secretローテーションでchecksumが変わらない")
	}
}

// TestRedisStandaloneAuth: managed standalone redisがrequirepass認証で構成されること
func TestRedisStandaloneAuth(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "redisauth"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	m := &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "ra", Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://ra.example.com/",
			Image:  "misskey/misskey:x",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			// Redis未指定 → managed standalone
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ra", Namespace: ns}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	// auth secret生成(passwordあり)
	authSec := &corev1.Secret{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameRedisAuthSecret(m), Namespace: ns}, authSec); err != nil {
		t.Fatalf("redis auth secret未生成: %v", err)
	}
	if len(authSec.Data["password"]) == 0 {
		t.Error("redis auth secretにpasswordが無い")
	}

	// redis STS: sh -c requirepass + REDIS_PASSWORD/REDISCLI_AUTH env
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameRedis(m), Namespace: ns}, sts); err != nil {
		t.Fatalf("redis STS未生成: %v", err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if len(c.Command) != 3 || c.Command[0] != "sh" || !strings.Contains(c.Command[2], `--requirepass "$REDIS_PASSWORD"`) {
		t.Errorf("redis command requirepass無し: %+v", c.Command)
	}
	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
	}
	if !envNames["REDIS_PASSWORD"] || !envNames["REDISCLI_AUTH"] {
		t.Errorf("redis env不足: %+v", c.Env)
	}

	// config: redisブロックにpass: ${REDIS_PASSWORD}
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameConfig(m), Namespace: ns}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["default.yml"], "pass: ${REDIS_PASSWORD}") {
		t.Errorf("default.ymlにredis pass無し:\n%s", cm.Data["default.yml"])
	}
}

// objStorageCR: external backend + objectStorage(sqlLike)のテスト用CR
func objStorageCR(name, ns string, auto *bool) *misskeyv1alpha1.Misskey {
	return &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:    "https://" + name + ".example.com/",
			Image:  "misskey/misskey:x",
			Search: misskeyv1alpha1.SearchSpec{Provider: misskeyv1alpha1.SearchSQLLike},
			Postgres: misskeyv1alpha1.PostgresSpec{External: &misskeyv1alpha1.ExternalPostgres{
				Host: "pg", Database: "d", User: "u",
				PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw"},
			}},
			Redis: misskeyv1alpha1.RedisSpec{External: &misskeyv1alpha1.ExternalRedis{Host: "redis"}},
			ObjectStorage: &misskeyv1alpha1.ObjectStorageSpec{
				Bucket: "media", Endpoint: "acct.r2.cloudflarestorage.com", Region: "auto", BaseURL: "https://cdn.example.com",
				AutoConfigure: auto,
				Credentials: misskeyv1alpha1.S3Credentials{
					AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "ak"},
					SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "sk"},
				},
			},
		},
	}
}

func objStorageJobs(t *testing.T, ctx context.Context, cl client.Client, m *misskeyv1alpha1.Misskey) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := cl.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels(selectorFor(m, "objstorage"))); err != nil {
		t.Fatalf("list objstorage jobs: %v", err)
	}
	return jobs.Items
}

func succeedJob(t *testing.T, ctx context.Context, cl client.Client, job *batchv1.Job) {
	t.Helper()
	job.Status.Succeeded = 1
	if err := cl.Status().Update(ctx, job); err != nil {
		t.Fatalf("job status update: %v", err)
	}
}

// TestObjectStorageGate: objectStorage+autoConfigureで、migration成功後にmeta書込Jobが作られ、
// その成功までapp/workerがgateされること
func TestObjectStorageGate(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "objgate"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: ns}, StringData: map[string]string{"ak": "A", "sk": "S"}}); err != nil {
		t.Fatalf("secret: %v", err)
	}
	m := objStorageCR("og", ns, nil)
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "og", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	// migration未完了→objstorage Jobもapp Deploymentも未生成
	if len(objStorageJobs(t, ctx, cl, m)) != 0 {
		t.Fatal("objstorage Job created before migration complete")
	}
	// migration成功
	mig := &batchv1.Job{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, mig); err != nil {
		t.Fatal(err)
	}
	succeedJob(t, ctx, cl, mig)
	reconcile()
	// objstorage Job生成、app未生成(gate)
	jobs := objStorageJobs(t, ctx, cl, m)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 objstorage Job after migration, got %d", len(jobs))
	}
	if exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) {
		t.Error("app Deployment created before objstorage Job succeeded")
	}
	// SQL ConfigMapが生成されている
	if !exists(ctx, cl, &corev1.ConfigMap{}, nameObjectStorageSQL(m), ns) {
		t.Error("objstorage SQL ConfigMap not created")
	}
	// objstorage成功→app/worker生成
	succeedJob(t, ctx, cl, &jobs[0])
	for i := 0; i < 2; i++ {
		reconcile()
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) {
		t.Error("app Deployment not created after objstorage Job succeeded")
	}
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	if !hasCondition(cur, "ObjectStorageConfigured", metav1.ConditionTrue) {
		t.Errorf("ObjectStorageConfigured!=True: %+v", cur.Status.Conditions)
	}
}

// TestObjectStorageAutoConfigureFalse: autoConfigure=falseでmeta書込Jobを作らず、
// app/workerをgateせず、conditionがUnmanagedになること
func TestObjectStorageAutoConfigureFalse(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "objoff"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: ns}, StringData: map[string]string{"ak": "A", "sk": "S"}}); err != nil {
		t.Fatalf("secret: %v", err)
	}
	m := objStorageCR("oo", ns, boolPtr(false))
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "oo", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	mig := &batchv1.Job{}
	if err := cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, mig); err != nil {
		t.Fatal(err)
	}
	succeedJob(t, ctx, cl, mig)
	for i := 0; i < 2; i++ {
		reconcile()
	}
	// meta書込Jobは作られない、app/workerはmigrationだけでgateされ生成される
	if len(objStorageJobs(t, ctx, cl, m)) != 0 {
		t.Error("autoConfigure=false must not create a meta Job")
	}
	if !exists(ctx, cl, &appsv1.Deployment{}, nameApp(m), ns) {
		t.Error("app Deployment must be created (not gated on objstorage) when autoConfigure=false")
	}
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	for _, c := range cur.Status.Conditions {
		if c.Type == "ObjectStorageConfigured" && c.Reason != "Unmanaged" {
			t.Errorf("expected Unmanaged reason, got %+v", c)
		}
	}
}

// TestObjectStorageChangeReRuns: 設定変更で新名Jobが作られ旧Jobが掃除されること
func TestObjectStorageChangeReRuns(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "objchg"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: ns}, StringData: map[string]string{"ak": "A", "sk": "S"}}); err != nil {
		t.Fatalf("secret: %v", err)
	}
	m := objStorageCR("oc", ns, nil)
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "oc", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	mig := &batchv1.Job{}
	_ = cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, mig)
	succeedJob(t, ctx, cl, mig)
	reconcile()
	first := objStorageJobs(t, ctx, cl, m)
	if len(first) != 1 {
		t.Fatalf("expected 1 job, got %d", len(first))
	}
	firstName := first[0].Name

	// bucketを変更→新名Job、旧掃除
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.ObjectStorage.Bucket = "media2"
	if err := cl.Update(ctx, cur); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcile()
	after := objStorageJobs(t, ctx, cl, m)
	if len(after) != 1 || after[0].Name == firstName {
		t.Errorf("expected a single new-named job after change, got %v (first %s)", jobNames(after), firstName)
	}
}

func jobNames(jobs []batchv1.Job) []string {
	out := make([]string, 0, len(jobs))
	for i := range jobs {
		out = append(out, jobs[i].Name)
	}
	return out
}

// TestObjectStorageRemovalCleanup: ブロック削除でJob+SQL ConfigMapが掃除されること(metaは不問)
func TestObjectStorageRemovalCleanup(t *testing.T) {
	ctx, cl, sch := setupEnvtest(t)
	ns := "objrm"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	if err := cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: ns}, StringData: map[string]string{"ak": "A", "sk": "S"}}); err != nil {
		t.Fatalf("secret: %v", err)
	}
	m := objStorageCR("orm", ns, nil)
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	r := &MisskeyReconciler{Client: cl, Scheme: sch}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "orm", Namespace: ns}}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		reconcile()
	}
	mig := &batchv1.Job{}
	_ = cl.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: ns}, mig)
	succeedJob(t, ctx, cl, mig)
	reconcile()
	if len(objStorageJobs(t, ctx, cl, m)) != 1 || !exists(ctx, cl, &corev1.ConfigMap{}, nameObjectStorageSQL(m), ns) {
		t.Fatal("setup: expected objstorage Job and SQL CM")
	}
	// objectStorageブロック削除
	cur := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.ObjectStorage = nil
	if err := cl.Update(ctx, cur); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcile()
	if len(objStorageJobs(t, ctx, cl, m)) != 0 {
		t.Error("objstorage Job not cleaned up after removal")
	}
	if exists(ctx, cl, &corev1.ConfigMap{}, nameObjectStorageSQL(m), ns) {
		t.Error("objstorage SQL ConfigMap not cleaned up after removal")
	}
}

// TestCELValidation: CRDのCEL(XValidation)がAPIサーバで常時強制されることを検証
// webhook非依存でimmutable(url/id/tenant)とcross-field整合が効くこと
func TestCELValidation(t *testing.T) {
	ctx, cl, _ := setupEnvtest(t)
	ns := "cel-test"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("ns: %v", err)
	}
	valid := func(name string) *misskeyv1alpha1.Misskey {
		return &misskeyv1alpha1.Misskey{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       misskeyv1alpha1.MisskeySpec{URL: "https://cel.example.com/", Image: "misskey/misskey:x", Tenant: "t1"},
		}
	}

	if err := cl.Create(ctx, valid("ok")); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	// immutable: update時にCELが拒否
	immutable := []struct {
		name   string
		mutate func(*misskeyv1alpha1.Misskey)
	}{
		{"url", func(m *misskeyv1alpha1.Misskey) { m.Spec.URL = "https://other.example.com/" }},
		{"idGenerationMethod", func(m *misskeyv1alpha1.Misskey) { m.Spec.IDGenerationMethod = "meid" }},
		{"tenant", func(m *misskeyv1alpha1.Misskey) { m.Spec.Tenant = "t2" }},
	}
	for _, tc := range immutable {
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, types.NamespacedName{Name: "ok", Namespace: ns}, cur); err != nil {
			t.Fatalf("get: %v", err)
		}
		tc.mutate(cur)
		if err := cl.Update(ctx, cur); !apierrors.IsInvalid(err) {
			t.Errorf("%s change must be rejected by CEL, got %v", tc.name, err)
		}
	}

	// recovery: 作成時指定はOK、以後の追加・変更・削除は拒否
	rec := func() *misskeyv1alpha1.PostgresRecovery {
		return &misskeyv1alpha1.PostgresRecovery{Source: misskeyv1alpha1.RecoverySource{
			DestinationPath: "s3://bk/misskey", ServerName: "old-db",
		}}
	}
	okRec := valid("ok-rec")
	okRec.Spec.Postgres.Recovery = rec()
	if err := cl.Create(ctx, okRec); err != nil {
		t.Fatalf("recovery at creation must be accepted: %v", err)
	}
	imp := func() *misskeyv1alpha1.PostgresImport {
		return &misskeyv1alpha1.PostgresImport{Source: misskeyv1alpha1.ImportSource{
			Host: "src-pg", Database: "d", User: "u",
			PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "p"},
		}}
	}
	okImp := valid("ok-imp")
	okImp.Spec.Postgres.Import = imp()
	if err := cl.Create(ctx, okImp); err != nil {
		t.Fatalf("import at creation must be accepted: %v", err)
	}
	recImmutable := []struct {
		name   string
		target string
		mutate func(*misskeyv1alpha1.Misskey)
	}{
		{"recovery add after creation", "ok", func(m *misskeyv1alpha1.Misskey) { m.Spec.Postgres.Recovery = rec() }},
		{"import add after creation", "ok", func(m *misskeyv1alpha1.Misskey) { m.Spec.Postgres.Import = imp() }},
		{"import host change", "ok-imp", func(m *misskeyv1alpha1.Misskey) { m.Spec.Postgres.Import.Source.Host = "other" }},
		{"import removal", "ok-imp", func(m *misskeyv1alpha1.Misskey) { m.Spec.Postgres.Import = nil }},
		{"recovery targetTime change", "ok-rec", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery.TargetTime = "2026-07-15T00:00:00Z"
		}},
		{"recovery serverName change", "ok-rec", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery.Source.ServerName = "other"
		}},
		{"recovery removal", "ok-rec", func(m *misskeyv1alpha1.Misskey) { m.Spec.Postgres.Recovery = nil }},
	}
	for _, tc := range recImmutable {
		cur := &misskeyv1alpha1.Misskey{}
		if err := cl.Get(ctx, types.NamespacedName{Name: tc.target, Namespace: ns}, cur); err != nil {
			t.Fatalf("get: %v", err)
		}
		tc.mutate(cur)
		if err := cl.Update(ctx, cur); !apierrors.IsInvalid(err) {
			t.Errorf("%s must be rejected by CEL, got %v", tc.name, err)
		}
	}
	// 無関係なupdateはrecovery付きでも通る(transition ruleの偽陽性検出)
	touched := &misskeyv1alpha1.Misskey{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "ok-rec", Namespace: ns}, touched); err != nil {
		t.Fatalf("get: %v", err)
	}
	touched.Labels = map[string]string{"touch": "1"}
	if err := cl.Update(ctx, touched); err != nil {
		t.Errorf("unrelated update with recovery must succeed: %v", err)
	}

	// cross-field: create時にCELが拒否
	extPG := &misskeyv1alpha1.ExternalPostgres{Host: "pg", Database: "d", User: "u",
		PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "p"}}
	minR := int32(5)
	cross := []struct {
		name  string
		build func(*misskeyv1alpha1.Misskey)
	}{
		{"pooler+external", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.External = extPG
			m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
		}},
		{"backup+external", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.External = extPG
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://b"}
		}},
		{"recovery+external", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.External = extPG
			m.Spec.Postgres.Recovery = rec()
		}},
		{"import+external", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.External = extPG
			m.Spec.Postgres.Import = imp()
		}},
		{"import+recovery", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery = rec()
			m.Spec.Postgres.Import = imp()
		}},
		{"image+imageFrom", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.ImageFrom = &misskeyv1alpha1.ImageFromSource{Channel: "stable"}
		}},
		{"imageもimageFromも無し", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Image = ""
		}},
		{"recovery+backup same path without serverName", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery = rec()
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://bk/misskey"}
		}},
		{"recovery+backup same path same serverName", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery = rec()
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://bk/misskey", ServerName: "old-db"}
		}},
		{"ha+external-redis", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Redis.External = &misskeyv1alpha1.ExternalRedis{Host: "r"}
			m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{}
		}},
		{"autoscaling min>max", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Worker.Autoscaling = &misskeyv1alpha1.AutoscalingSpec{MinReplicas: &minR, MaxReplicas: 3}
		}},
		{"redis role external+ha", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{JobQueue: &misskeyv1alpha1.RedisRole{
				External: &misskeyv1alpha1.ExternalRedis{Host: "r"}, HA: &misskeyv1alpha1.RedisHA{},
			}}
		}},
		{"external redis sentinels without masterName", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Redis.External = &misskeyv1alpha1.ExternalRedis{
				Host: "r", Sentinels: []misskeyv1alpha1.RedisHostPort{{Host: "s1"}},
			}
		}},
		// pattern validation(schema)
		{"invalid maxMemory", func(m *misskeyv1alpha1.Misskey) { m.Spec.Redis.MaxMemory = "lots" }},
		{"invalid backup schedule", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://b", Schedule: "not-cron"}
		}},
		{"invalid monitoring interval", func(m *misskeyv1alpha1.Misskey) { m.Spec.Monitoring.Interval = "30" }},
		// objectStorage
		{"objectStorage without bucket", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.ObjectStorage = &misskeyv1alpha1.ObjectStorageSpec{
				Credentials: misskeyv1alpha1.S3Credentials{
					AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "ak"},
					SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "sk"},
				},
			}
		}},
		{"objectStorage endpoint with scheme", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.ObjectStorage = &misskeyv1alpha1.ObjectStorageSpec{
				Bucket: "b", Endpoint: "https://s3.example.com",
				Credentials: misskeyv1alpha1.S3Credentials{
					AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "ak"},
					SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "sk"},
				},
			}
		}},
		{"objectStorage extraColumns invalid identifier", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.ObjectStorage = &misskeyv1alpha1.ObjectStorageSpec{
				Bucket:       "b",
				ExtraColumns: map[string]string{"bad col; DROP": "x"},
				Credentials: misskeyv1alpha1.S3Credentials{
					AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "ak"},
					SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "sk"},
				},
			}
		}},
	}
	for i, tc := range cross {
		m := valid(fmt.Sprintf("cross-%d", i))
		tc.build(m)
		if err := cl.Create(ctx, m); !apierrors.IsInvalid(err) {
			t.Errorf("%s must be rejected by CEL, got %v", tc.name, err)
		}
	}

	// 肯定: 同一destinationPathでもserverName相違なら許可, 別destinationPathはserverName無しで許可
	positive := []struct {
		name  string
		build func(*misskeyv1alpha1.Misskey)
	}{
		{"recovery+backup same path distinct serverName", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery = rec()
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://bk/misskey", ServerName: "new-db"}
		}},
		{"recovery+backup different path", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Postgres.Recovery = rec()
			m.Spec.Postgres.Backup = &misskeyv1alpha1.PostgresBackup{DestinationPath: "s3://bk2/misskey"}
		}},
		{"imageFromのみ", func(m *misskeyv1alpha1.Misskey) {
			m.Spec.Image = ""
			m.Spec.ImageFrom = &misskeyv1alpha1.ImageFromSource{Channel: "stable"}
		}},
	}
	for i, tc := range positive {
		m := valid(fmt.Sprintf("pos-%d", i))
		tc.build(m)
		if err := cl.Create(ctx, m); err != nil {
			t.Errorf("%s must be accepted, got %v", tc.name, err)
		}
	}
}
