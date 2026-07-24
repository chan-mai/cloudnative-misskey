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
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// migrationChecksum: migration Jobの入力checksum annotation
// migrate config本文+concurrently flag+参照Secret版数。認証情報ローテで失敗Jobも作り直される
// image変更はJob名(imageHash)で別Jobになるため含めない
func (r *MisskeyReconciler) migrationChecksum(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (map[string]string, error) {
	versions, err := r.referencedSecretVersions(ctx, m, p)
	if err != nil {
		return nil, err
	}
	parts := []string{renderDefaultYML(m, migratePlan(m, p)), strconv.FormatBool(migrationConcurrentIndexes(m))}
	parts = append(parts, versions...)
	return checksumAnnotation(parts...), nil
}

// buildMigrationJob: `pnpm run migrate`を1回だけ実行するJob。app/workerと同じinit/volumeを流用
func buildMigrationJob(m *misskeyv1beta1.Misskey, p plan, annotations map[string]string) *batchv1.Job {
	var env []corev1.EnvVar
	// index作成migrationを CREATE INDEX CONCURRENTLY にし、note等の巨大表への
	// 書込ブロック(SHAREロック)を避ける。ormconfig.jsがmigrationsTransactionMode='each'へ切替
	if migrationConcurrentIndexes(m) {
		env = append(env, corev1.EnvVar{Name: "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY", Value: "1"})
	}
	pod := corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		RestartPolicy:                corev1.RestartPolicyOnFailure,
		ImagePullSecrets:             m.Spec.ImagePullSecrets,
		SecurityContext:              nonRootPodSecurityContext(runtimeUID(m)),
		InitContainers:               misskeyInitContainers(m, p),
		Containers: []corev1.Container{{
			Name:            "migrate",
			Image:           m.Spec.Image,
			Command:         runtimeMigrateCommand(m),
			Env:             env,
			SecurityContext: restrictedContainerSecurityContext(),
			Resources:       resourcesOr(corev1.ResourceRequirements{}, "100m", "400Mi", "800Mi"),
			VolumeMounts:    misskeyConfigMounts(m),
		}},
		Volumes: misskeyVolumes(m, nameMigrateConfig(m)),
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: nameMigrate(m), Namespace: m.Namespace,
			Labels: labelsFor(m, "migrate"),
			// 失敗時の再生成判定用(reconcileMigration参照)
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(20), // DB起動待ちの猶予
			Parallelism:  int32Ptr(1),
			Completions:  int32Ptr(1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(m, "migrate")},
				Spec:       pod,
			},
		},
	}
}

// reconcileMigration: 現行imageのmigration Jobをcreate-if-absentで用意し、旧versionを掃除する
// Jobはtemplate immutableなのでCreateOrUpdateは使わない。戻り値は完了(Succeeded>=1)か
func (r *MisskeyReconciler) reconcileMigration(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (bool, error) {
	if err := r.cleanupOldMigrationJobs(ctx, m); err != nil {
		return false, err
	}
	checksum, err := r.migrationChecksum(ctx, m, p)
	if err != nil {
		return false, err
	}
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: m.Namespace}, job)
	if apierrors.IsNotFound(err) {
		job = buildMigrationJob(m, p, checksum)
		if err := controllerutil.SetControllerReference(m, job, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, job); err != nil {
			return false, err
		}
		r.event(m, corev1.EventTypeNormal, "MigrationStarted", "Migrate", "created migration Job %s (image %s)", job.Name, m.Spec.Image)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// 失敗Jobは入力(DB接続先config/concurrently flag)が変わった時のみ削除して作り直す
	// (次のreconcileでcreate-if-absentが再生成)。同一入力での失敗は保持し手動削除で再試行とする
	// CREATE INDEX CONCURRENTLY失敗時のinvalid index堆積等を防ぐため無条件リトライはしない
	if job.Status.Succeeded == 0 && job.Status.Failed >= 1 {
		if job.Annotations[configChecksumAnnotation] != checksum[configChecksumAnnotation] {
			policy := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			r.event(m, corev1.EventTypeNormal, "MigrationRetried", "Migrate", "recreated failed migration Job %s after a configuration change", job.Name)
			return false, nil
		}
		r.event(m, corev1.EventTypeWarning, "MigrationFailed", "Migrate", "migration Job %s failed (%d); delete the Job to retry with the same configuration", job.Name, job.Status.Failed)
	}
	return job.Status.Succeeded >= 1, nil
}

// preBackupEnabled: spec.migration.preBackupが有効か
func preBackupEnabled(m *misskeyv1beta1.Misskey) bool {
	return m.Spec.Migration.PreBackup != nil && *m.Spec.Migration.PreBackup
}

// buildPreMigrationBackup: migration前のon-demand CNPG Backup
func buildPreMigrationBackup(m *misskeyv1beta1.Misskey) *unstructured.Unstructured {
	b := &unstructured.Unstructured{}
	b.SetGroupVersionKind(cnpgBackupGVK)
	b.SetName(namePreBackup(m))
	b.SetNamespace(m.Namespace)
	b.SetLabels(labelsFor(m, "premigrate"))
	b.Object["spec"] = map[string]any{
		"cluster": map[string]any{"name": nameDB(m)},
	}
	return b
}

// reconcilePreMigrationBackup: migration Job作成前にon-demandバックアップの完了をgateする
// 失敗したmigrationをpostgres.recoveryで巻き戻せる状態を担保する。戻り値はgate通過可否
func (r *MisskeyReconciler) reconcilePreMigrationBackup(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (bool, error) {
	if !preBackupEnabled(m) || !p.dbManaged || m.Spec.Postgres.Backup == nil {
		return true, nil
	}
	if err := r.cleanupOldPreMigrationBackups(ctx, m); err != nil {
		return false, err
	}
	// 現行imageのmigration Jobが既にあればgateしない(導入前から進行中のmigrationを塞がない)
	if err := r.Get(ctx, types.NamespacedName{Name: nameMigrate(m), Namespace: m.Namespace}, &batchv1.Job{}); err == nil {
		return true, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	backup := &unstructured.Unstructured{}
	backup.SetGroupVersionKind(cnpgBackupGVK)
	err := r.Get(ctx, types.NamespacedName{Name: namePreBackup(m), Namespace: m.Namespace}, backup)
	if apierrors.IsNotFound(err) {
		backup = buildPreMigrationBackup(m)
		if err := controllerutil.SetControllerReference(m, backup, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, backup); err != nil {
			return false, err
		}
		r.event(m, corev1.EventTypeNormal, "PreBackupStarted", "Migrate", "created pre-migration Backup %s (image %s)", backup.GetName(), m.Spec.Image)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	switch phase {
	case "completed":
		return true, nil
	case "failed":
		// migration Job失敗時と同じセマンティクス, Backup削除で再試行
		r.event(m, corev1.EventTypeWarning, "PreBackupFailed", "Migrate", "pre-migration Backup %s failed; delete it to retry", backup.GetName())
		return false, nil
	default:
		return false, nil
	}
}

// cleanupOldPreMigrationBackups: 現行image以外のpre-migration Backupを削除
func (r *MisskeyReconciler) cleanupOldPreMigrationBackups(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(cnpgBackupListGVK)
	if err := r.List(ctx, &list, client.InNamespace(m.Namespace), client.MatchingLabels(selectorFor(m, "premigrate"))); err != nil {
		return err
	}
	current := namePreBackup(m)
	for i := range list.Items {
		b := &list.Items[i]
		if b.GetName() == current {
			continue
		}
		if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// cleanupOldMigrationJobs: 現行image以外のmigration Jobを削除(古いmigrationは適用済みで不要)
func (r *MisskeyReconciler) cleanupOldMigrationJobs(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels(selectorFor(m, "migrate"))); err != nil {
		return err
	}
	current := nameMigrate(m)
	policy := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Name == current {
			continue
		}
		if err := r.Delete(ctx, j, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
