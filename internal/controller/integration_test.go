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
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
