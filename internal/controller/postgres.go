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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

var (
	cnpgClusterGVK = schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster",
	}
	cnpgScheduledBackupGVK = schema.GroupVersionKind{
		Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup",
	}
)

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
