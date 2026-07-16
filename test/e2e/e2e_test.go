//go:build e2e

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

// kindクラスタ(cert-manager+CNPG+webhook入りoperator導入済み前提、hack/e2e.sh参照)に対し、
// 実misskey imageでCR作成→webhook defaulting→CNPG provisioning→実migration→Ready→削除GCを検証する
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

const (
	ns           = "cnm-e2e"
	name         = "e2e"
	misskeyImage = "misskey/misskey:2026.6.0"
)

func newClient(t *testing.T) client.Client {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := misskeyv1beta1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return cl
}

// dumpDiagnostics: 失敗時のCI調査用にCR conditionsとpod状態を出力
func dumpDiagnostics(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	m := &misskeyv1beta1.Misskey{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, m); err == nil {
		t.Logf("phase=%s databaseHost=%s", m.Status.Phase, m.Status.DatabaseHost)
		for _, c := range m.Status.Conditions {
			t.Logf("condition %s=%s (%s): %s", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	var pods corev1.PodList
	if err := cl.List(ctx, &pods, client.InNamespace(ns)); err == nil {
		for i := range pods.Items {
			p := &pods.Items[i]
			t.Logf("pod %s phase=%s", p.Name, p.Status.Phase)
			for _, cs := range p.Status.ContainerStatuses {
				t.Logf("  container %s ready=%v restarts=%d state=%+v", cs.Name, cs.Ready, cs.RestartCount, cs.State)
			}
		}
	}
}

func TestE2E(t *testing.T) {
	ctx := context.Background()
	cl := newClient(t)

	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ns: %v", err)
	}

	// ダミーのobject storage資格情報。実S3疎通はせず、meta書込Jobが実CNPGへ
	// UPDATE meta を実行できること(psql/\getenv/SQL構造)を検証する
	if err := cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: ns},
		StringData: map[string]string{"ACCESS_KEY_ID": "dummy", "SECRET_ACCESS_KEY": "dummy"},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("s3 secret: %v", err)
	}

	m := &misskeyv1beta1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: misskeyv1beta1.MisskeySpec{
			URL:           "https://e2e.example.com/",
			Image:         misskeyImage,
			SetupPassword: &misskeyv1beta1.SetupPasswordSpec{},
			Search:        misskeyv1beta1.SearchSpec{Provider: misskeyv1beta1.SearchSQLLike},
			Postgres:      misskeyv1beta1.PostgresSpec{Instances: 1, Storage: resource.MustParse("2Gi")},
			Redis:         misskeyv1beta1.RedisSpec{Storage: resource.MustParse("1Gi")},
			ObjectStorage: &misskeyv1beta1.ObjectStorageSpec{
				Bucket:   "e2e-media",
				Endpoint: "s3.example.com",
				Region:   "auto",
				BaseURL:  "https://media.example.com",
				Credentials: misskeyv1beta1.S3Credentials{
					AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3-creds"}, Key: "ACCESS_KEY_ID"},
					SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3-creds"}, Key: "SECRET_ACCESS_KEY"},
				},
			},
		},
	}
	if err := cl.Create(ctx, m); err != nil {
		t.Fatalf("create misskey: %v", err)
	}

	waitFor := func(desc string, timeout time.Duration, fn func(context.Context) (bool, error)) {
		t.Helper()
		t.Logf("waiting: %s", desc)
		if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, fn); err != nil {
			dumpDiagnostics(t, ctx, cl)
			t.Fatalf("%s: %v", desc, err)
		}
	}

	// webhook defaulting(cert-manager+webhook結線の証明): tenant未指定→namespace確定
	got := &misskeyv1beta1.Misskey{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Tenant != ns {
		t.Errorf("webhook defaulting: tenant=%q, want %q", got.Spec.Tenant, ns)
	}

	jobSucceeded := func(component string) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) {
			var jobs batchv1.JobList
			if err := cl.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{
				"app.kubernetes.io/instance":  name,
				"app.kubernetes.io/component": component,
			}); err != nil {
				return false, nil
			}
			for i := range jobs.Items {
				if jobs.Items[i].Status.Succeeded >= 1 {
					return true, nil
				}
			}
			return false, nil
		}
	}

	// 実DBへの実migration完了(CNPG provisioning込み)
	waitFor("migration Job succeeded", 12*time.Minute, jobSucceeded("migrate"))

	// meta書込Jobが実CNPGへUPDATE metaを実行して成功すること
	// (psql/\getenv/INSERT ON CONFLICT+UPDATEの実挙動検証。unit/envtestでは不可)
	waitFor("object storage meta Job succeeded", 5*time.Minute, jobSucceeded("objstorage"))

	// 全subsystem Ready(app/worker実起動、probe通過)
	waitFor("Ready=True", 12*time.Minute, func(ctx context.Context) (bool, error) {
		cur := &misskeyv1beta1.Misskey{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cur); err != nil {
			return false, nil
		}
		for _, c := range cur.Status.Conditions {
			if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
				return cur.Status.Phase == "Running", nil
			}
		}
		return false, nil
	})

	// 解決済み接続先(pooler無効なのでCNPGのrwサービス)
	cur := &misskeyv1beta1.Misskey{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.DatabaseHost != name+"-db-rw" {
		t.Errorf("databaseHost=%q, want %s-db-rw", cur.Status.DatabaseHost, name)
	}

	// managed redisがrequirepass認証で構成され、app/workerが認証接続してReadyに到達したことを実クラスタで確認
	// (Ready到達自体がpassword一致のend-to-end証明。加えてsecret/STS commandを明示検証)
	authSec := &corev1.Secret{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name + "-redis-auth", Namespace: ns}, authSec); err != nil {
		t.Errorf("redis auth secret not created: %v", err)
	} else if len(authSec.Data["password"]) == 0 {
		t.Error("redis auth secret has no password")
	}
	redisSTS := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name + "-redis", Namespace: ns}, redisSTS); err != nil {
		t.Errorf("get redis STS: %v", err)
	} else if cmd := redisSTS.Spec.Template.Spec.Containers[0].Command; len(cmd) != 3 || !strings.Contains(cmd[2], "--requirepass") {
		t.Errorf("redis STS missing requirepass: %+v", cmd)
	}

	// 削除→finalizer→子リソースGC
	if err := cl.Delete(ctx, cur); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor("CR deleted", 3*time.Minute, func(ctx context.Context) (bool, error) {
		err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &misskeyv1beta1.Misskey{})
		return apierrors.IsNotFound(err), nil
	})
	waitFor("app Deployment GC", 3*time.Minute, func(ctx context.Context) (bool, error) {
		err := cl.Get(ctx, types.NamespacedName{Name: name + "-app", Namespace: ns}, &appsv1.Deployment{})
		return apierrors.IsNotFound(err), nil
	})
}
