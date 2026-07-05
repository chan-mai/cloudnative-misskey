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

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
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
	}
	for i, tc := range cross {
		m := valid(fmt.Sprintf("cross-%d", i))
		tc.build(m)
		if err := cl.Create(ctx, m); !apierrors.IsInvalid(err) {
			t.Errorf("%s must be rejected by CEL, got %v", tc.name, err)
		}
	}
}
