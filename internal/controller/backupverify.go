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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// backupVerifyDue: 次の復元検証が実行時期か
func backupVerifyDue(m *misskeyv1beta1.Misskey, interval time.Duration, now time.Time) bool {
	v := m.Status.BackupVerification
	if v == nil || v.LastVerifiedTime.IsZero() {
		return true
	}
	return now.Sub(v.LastVerifiedTime.Time) >= interval
}

// buildDBVerifyCluster: 自前バックアップからbootstrap復元する使い捨てCNPG Cluster
// backupセクションを持たない(WALアーカイブせず復元元アーカイブと衝突しない)
func buildDBVerifyCluster(m *misskeyv1beta1.Misskey) *unstructured.Unstructured {
	pg := m.Spec.Postgres
	b := pg.Backup
	storageSize := quantityOr(pg.Storage, "20Gi")

	inheritedLabels := map[string]any{}
	for k, v := range labelsFor(m, "postgres-verify") {
		inheritedLabels[k] = v
	}

	barman := map[string]any{
		"destinationPath": b.DestinationPath,
		"serverName":      stringOr(b.ServerName, nameDB(m)),
		"wal":             map[string]any{"maxParallel": int64(8)},
	}
	if b.EndpointURL != "" {
		barman["endpointURL"] = b.EndpointURL
	}
	if b.S3Credentials != nil {
		barman["s3Credentials"] = s3CredentialsMap(b.S3Credentials)
	}

	spec := map[string]any{
		"instances": int64(1),
		"imageName": stringOr(pg.Image, "ghcr.io/cloudnative-pg/postgresql:17"),
		"inheritedMetadata": map[string]any{
			"labels": inheritedLabels,
		},
		"storage": map[string]any{
			"size": storageSize.String(),
		},
		"bootstrap": map[string]any{
			"recovery": map[string]any{"source": recoveryOriginName},
		},
		"externalClusters": []any{
			map[string]any{"name": recoveryOriginName, "barmanObjectStore": barman},
		},
	}
	if pg.StorageClassName != nil && *pg.StorageClassName != "" {
		spec["storage"].(map[string]any)["storageClass"] = *pg.StorageClassName
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(nameDBVerify(m))
	cluster.SetNamespace(m.Namespace)
	cluster.SetLabels(labelsFor(m, "postgres-verify"))
	cluster.Object["spec"] = spec
	return cluster
}

func verifyClusterRef(m *misskeyv1beta1.Misskey) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgClusterGVK)
	u.SetName(nameDBVerify(m))
	u.SetNamespace(m.Namespace)
	return u
}

// reconcileBackupVerify: backup.verifyの周期で使い捨てClusterによる復元検証を回す
// due→作成, ready→Succeeded記録+削除, timeout→Failed記録+削除。進行検知はdrift resyncで足りる
func (r *MisskeyReconciler) reconcileBackupVerify(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	b := m.Spec.Postgres.Backup
	if b == nil || b.Verify == nil {
		return r.deleteIfExists(ctx, verifyClusterRef(m))
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	err := r.Get(ctx, types.NamespacedName{Name: nameDBVerify(m), Namespace: m.Namespace}, cluster)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	now := time.Now()

	if apierrors.IsNotFound(err) {
		interval := b.Verify.Interval.Duration
		if interval <= 0 {
			interval = 168 * time.Hour
		}
		if !backupVerifyDue(m, interval, now) {
			return nil
		}
		if err := r.applySSA(ctx, m, buildDBVerifyCluster(m)); err != nil {
			return err
		}
		r.event(m, corev1.EventTypeNormal, "BackupVerifyStarted", "VerifyBackup", "created restore test cluster %s", nameDBVerify(m))
		return nil
	}

	timeout := b.Verify.Timeout.Duration
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	switch {
	case ready >= 1:
		if err := r.recordBackupVerification(ctx, m, "Succeeded", "restore test cluster became ready", now); err != nil {
			return err
		}
		r.event(m, corev1.EventTypeNormal, "BackupVerified", "VerifyBackup", "restore test from %s succeeded", b.DestinationPath)
		return r.deleteIfExists(ctx, verifyClusterRef(m))
	case now.Sub(cluster.GetCreationTimestamp().Time) > timeout:
		msg := fmt.Sprintf("restore test cluster not ready within %s", timeout)
		if err := r.recordBackupVerification(ctx, m, "Failed", msg, now); err != nil {
			return err
		}
		r.event(m, corev1.EventTypeWarning, "BackupVerifyFailed", "VerifyBackup", "%s", msg)
		return r.deleteIfExists(ctx, verifyClusterRef(m))
	default:
		return nil
	}
}

// recordBackupVerification: 検証結果のみをstatusへ反映
// 単一status writer原則(updateStatus)の例外, 使い捨てCluster削除後は結果がここにしか残らないため完了時点で書く
func (r *MisskeyReconciler) recordBackupVerification(ctx context.Context, m *misskeyv1beta1.Misskey, result, message string, now time.Time) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &misskeyv1beta1.Misskey{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(m), cur); err != nil {
			return err
		}
		cur.Status.BackupVerification = &misskeyv1beta1.BackupVerificationStatus{
			LastVerifiedTime: metav1.NewTime(now),
			Result:           result,
			Message:          message,
		}
		if err := r.Status().Update(ctx, cur); err != nil {
			return err
		}
		// 同一reconcile内のdue再判定と後続updateStatusの上書き防止用に手元へも反映
		m.Status.BackupVerification = cur.Status.BackupVerification
		return nil
	})
}
