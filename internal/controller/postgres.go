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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
)

var (
	cnpgClusterGVK = schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster",
	}
	cnpgScheduledBackupGVK = schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup",
	}
	cnpgPoolerGVK = schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler",
	}
)

// poolerEnabled: spec.postgres.poolerが在ればenabled(default true)
func poolerEnabled(m *misskeyv1alpha1.Misskey) bool {
	return m.Spec.Postgres.Pooler != nil
}

// readOffloadActive: replicaが居る(instances>=2)かつreadOffloadがopt-outされていない
func readOffloadActive(m *misskeyv1alpha1.Misskey) bool {
	return int32OrDefault(m.Spec.Postgres.Instances, 1) >= 2 && boolOr(m.Spec.Postgres.ReadOffload, true)
}

// managedデータベース用にCNPG Cluster(とScheduledBackup)を作成/更新
// spec.postgres.external設定時はno-op
func (r *MisskeyReconciler) reconcilePostgres(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	pg := m.Spec.Postgres
	storageSize := quantityOr(pg.Storage, "20Gi")

	initdb := map[string]any{
		"database": stringOr(pg.Database, "misskey"),
		"owner":    stringOr(pg.Owner, "misskey"),
	}
	// PGroonga全文検索では、init時にアプリケーションDBへ拡張を作成
	// postgres.imageNameでPGroonga有効イメージが必要。既定イメージだとCNPGのbootstrapが黙らず明示的に失敗する
	if m.Spec.Search.Provider == misskeyv1alpha1.SearchSQLPgroonga {
		initdb["postInitApplicationSQL"] = []any{"CREATE EXTENSION IF NOT EXISTS pgroonga"}
	}

	// CNPGが生成する全リソース(DB pod含む)に自前のラベルを継承させる
	inheritedLabels := map[string]any{}
	for k, v := range labelsFor(m, "postgres") {
		inheritedLabels[k] = v
	}

	spec := map[string]any{
		"instances": int64(int32OrDefault(pg.Instances, 1)),
		"imageName": stringOr(pg.ImageName, "ghcr.io/cloudnative-pg/postgresql:17"),
		"inheritedMetadata": map[string]any{
			"labels": inheritedLabels,
		},
		"storage": map[string]any{
			"size": storageSize.String(),
		},
		"bootstrap": map[string]any{
			"initdb": initdb,
		},
	}

	if pg.StorageClassName != nil && *pg.StorageClassName != "" {
		spec["storage"].(map[string]any)["storageClass"] = *pg.StorageClassName
	}

	if len(pg.Parameters) > 0 {
		params := map[string]any{}
		for k, v := range pg.Parameters {
			params[k] = v
		}
		spec["postgresql"] = map[string]any{"parameters": params}
	}

	if res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pg.Resources); err == nil && len(res) > 0 {
		spec["resources"] = res
	}

	if b := pg.Backup; b != nil {
		barman := map[string]any{
			"destinationPath": b.DestinationPath,
			"wal":             map[string]any{"compression": "gzip"},
			"data":            map[string]any{"compression": "gzip"},
		}
		if b.EndpointURL != "" {
			barman["endpointURL"] = b.EndpointURL
		}
		if b.S3Credentials != nil {
			barman["s3Credentials"] = map[string]any{
				"accessKeyId": map[string]any{
					"name": b.S3Credentials.AccessKeyID.Name,
					"key":  b.S3Credentials.AccessKeyID.Key,
				},
				"secretAccessKey": map[string]any{
					"name": b.S3Credentials.SecretAccessKey.Name,
					"key":  b.S3Credentials.SecretAccessKey.Key,
				},
			}
		}
		spec["backup"] = map[string]any{
			"barmanObjectStore": barman,
			"retentionPolicy":   stringOr(b.RetentionPolicy, "30d"),
		}
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(nameDB(m))
	cluster.SetNamespace(m.Namespace)
	cluster.SetLabels(labelsFor(m, "postgres"))
	cluster.Object["spec"] = spec
	if err := r.applySSA(ctx, m, cluster); err != nil {
		return err
	}

	// 任意のスケジュールバックアップ
	if pg.Backup == nil || pg.Backup.Schedule == "" {
		return nil
	}
	sb := &unstructured.Unstructured{}
	sb.SetGroupVersionKind(cnpgScheduledBackupGVK)
	sb.SetName(nameDB(m))
	sb.SetNamespace(m.Namespace)
	sb.SetLabels(labelsFor(m, "postgres"))
	sb.Object["spec"] = map[string]any{
		"schedule":             pg.Backup.Schedule,
		"backupOwnerReference": "self",
		"cluster":              map[string]any{"name": nameDB(m)},
	}
	return r.applySSA(ctx, m, sb)
}

// reconcilePoolers: pooler有効時にrw(書込)と、read offload有効時ro(読取)のCNPG Poolerをapply
// 無効化/instances<2へのdowngrade時は該当プーラーをcleanup
func (r *MisskeyReconciler) reconcilePoolers(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	// rwプーラー: pooler有効時
	if poolerEnabled(m) {
		if err := r.applySSA(ctx, m, buildPooler(m, nameDBPoolerRW(m), "rw")); err != nil {
			return err
		}
	} else if err := r.deletePooler(ctx, m, nameDBPoolerRW(m)); err != nil {
		return err
	}

	// roプーラー: pooler有効かつreplicaへread offloadする時のみ
	if poolerEnabled(m) && readOffloadActive(m) {
		return r.applySSA(ctx, m, buildPooler(m, nameDBPoolerRO(m), "ro"))
	}
	return r.deletePooler(ctx, m, nameDBPoolerRO(m))
}

// buildPooler: 指定type(rw/ro)のCNPG Pooler unstructuredを生成
// 呼び出し側でPooler != nilを保証
func buildPooler(m *misskeyv1alpha1.Misskey, name, poolerType string) *unstructured.Unstructured {
	pc := m.Spec.Postgres.Pooler

	// PgBouncerパラメータ: デフォルトにユーザー指定をmerge
	params := map[string]any{
		"max_client_conn":   "1000",
		"default_pool_size": "25",
		// transaction poolingでMisskey(node-postgres)が送るstartup paramを無視
		// 無いとstatement_timeoutで"unsupported startup parameter"となりapp接続失敗
		"ignore_startup_parameters": "statement_timeout,extra_float_digits,search_path,options,idle_in_transaction_session_timeout",
	}
	for k, v := range pc.Parameters {
		params[k] = v
	}

	// pooler podへlabel付与。CNPGはapp.kubernetes.io/{name,instance,component,managed-by}を
	// クラスタ名等で上書きするが、tenant等の独自labelは残りobservability/tenant集計に効く
	// NP到達性はinstance labelに頼れないためtenancy.goがcnpg.io/clusterで別途許可
	podLabels := map[string]any{}
	for k, v := range labelsFor(m, "postgres-pooler") {
		podLabels[k] = v
	}
	template := map[string]any{
		"metadata": map[string]any{"labels": podLabels},
	}
	if res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pc.Resources); err == nil && len(res) > 0 {
		template["spec"] = map[string]any{
			"containers": []any{map[string]any{"name": "pgbouncer", "resources": res}},
		}
	}

	spec := map[string]any{
		"cluster":   map[string]any{"name": nameDB(m)},
		"instances": int64(int32OrDefault(pc.Instances, 2)),
		"type":      poolerType,
		"template":  template,
		"pgbouncer": map[string]any{
			"poolMode":   stringOr(pc.PoolMode, "transaction"),
			"parameters": params,
		},
	}

	pooler := &unstructured.Unstructured{}
	pooler.SetGroupVersionKind(cnpgPoolerGVK)
	pooler.SetName(name)
	pooler.SetNamespace(m.Namespace)
	pooler.SetLabels(labelsFor(m, "postgres-pooler"))
	pooler.Object["spec"] = spec
	return pooler
}

// deletePooler: 指定名のPoolerが在れば削除(無効化/downgrade時のcleanup)
func (r *MisskeyReconciler) deletePooler(ctx context.Context, m *misskeyv1alpha1.Misskey, name string) error {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(cnpgPoolerGVK)
	p.SetName(name)
	p.SetNamespace(m.Namespace)
	return r.deleteIfExists(ctx, p)
}
