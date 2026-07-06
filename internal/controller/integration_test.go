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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
	if !exists(ctx, cl, &appsv1.Deployment{}, nameMaintenance(m), ns) {
		t.Error("maintenance Deployment未生成")
	}
	if !exists(ctx, cl, &corev1.ConfigMap{}, nameMaintenanceHTML(m), ns) {
		t.Error("maintenance HTML ConfigMap未生成")
	}
	if !exists(ctx, cl, &networkingv1.Ingress{}, m.Name, ns) {
		t.Error("Ingress未生成")
	}

	// maintenanceのみ無効化 → maintenance側だけ掃除、proxyは残る
	update(func(c *misskeyv1alpha1.Misskey) { c.Spec.Proxy.Maintenance.Enabled = boolPtr(false) })
	reconcile()
	if exists(ctx, cl, &appsv1.Deployment{}, nameMaintenance(m), ns) {
		t.Error("maintenance無効化後もDeploymentが残存")
	}
	if exists(ctx, cl, &corev1.Service{}, nameMaintenance(m), ns) {
		t.Error("maintenance無効化後もServiceが残存")
	}
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
}
