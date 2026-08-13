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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

const sensitiveDetectorComponent = "sensitive-detector"

func (r *MisskeyReconciler) reconcileSensitiveDetectorSecret(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	detector := m.Spec.SensitiveDetector
	if detector == nil || detector.External != nil || detector.APIKeySecret != nil {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameSensitiveDetector(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, sensitiveDetectorComponent)
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data[sensitiveDetectorAPIKeyID]; !ok || rotationRequested(m, secret) {
			key, err := randomHex(32)
			if err != nil {
				return err
			}
			secret.Data[sensitiveDetectorAPIKeyID] = []byte(key)
		}
		markRotation(m, secret)
		return nil
	})
}

func renderSensitiveDetectorConfig() string {
	return `export default {
  port: 3009,
  host: '0.0.0.0',
  modelDir: '/models',
  apiKey: process.env.SENSITIVE_DETECTOR_API_KEY,
};
`
}

func buildSensitiveDetectorPodSpec(m *misskeyv1beta1.Misskey, p plan) corev1.PodSpec {
	detector := m.Spec.SensitiveDetector
	return corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		ImagePullSecrets:             m.Spec.ImagePullSecrets,
		SecurityContext:              nonRootPodSecurityContext(genericNonRootUID),
		NodeSelector:                 detector.NodeSelector,
		Tolerations:                  detector.Tolerations,
		Containers: []corev1.Container{{
			Name:            sensitiveDetectorComponent,
			Image:           stringOr(detector.Image, "ghcr.io/misskey-dev/sensitive-detector:0.0.2"),
			SecurityContext: restrictedContainerSecurityContext(),
			Resources:       resourcesOr(detector.Resources, "250m", "512Mi", "2Gi"),
			Env:             []corev1.EnvVar{secretEnv(sensitiveDetectorAPIKeyID, p.sensitiveDetectorAPIKeySel)},
			Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: sensitiveDetectorPort}},
			StartupProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")}},
				PeriodSeconds:    5,
				FailureThreshold: 12,
				TimeoutSeconds:   3,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")}},
				PeriodSeconds:    10,
				FailureThreshold: 3,
				TimeoutSeconds:   3,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")}},
				PeriodSeconds:    20,
				FailureThreshold: 3,
				TimeoutSeconds:   3,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: "/config/config.mjs", SubPath: "config.mjs", ReadOnly: true},
				tmpMount(),
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: nameSensitiveDetector(m)}}}},
			tmpVolume(),
		},
	}
}

func (r *MisskeyReconciler) reconcileSensitiveDetector(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	detector := m.Spec.SensitiveDetector
	if detector == nil || !p.sensitiveDetectorManaged {
		return nil
	}

	name := nameSensitiveDetector(m)
	config := renderSensitiveDetectorConfig()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, cm, func() error {
		cm.Labels = labelsFor(m, sensitiveDetectorComponent)
		cm.Data = map[string]string{"config.mjs": config}
		return nil
	}); err != nil {
		return err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, sensitiveDetectorComponent)
		svc.Spec.Selector = selectorFor(m, sensitiveDetectorComponent)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       sensitiveDetectorPort,
			TargetPort: intstr.FromInt32(sensitiveDetectorPort),
		}}
		return nil
	}); err != nil {
		return err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, dep, func() error {
		secretVersion, err := r.secretVersion(ctx, m.Namespace, p.sensitiveDetectorAPIKeySel.Name)
		if err != nil {
			return err
		}
		pod := buildSensitiveDetectorPodSpec(m, p)
		setDeployment(dep, m, sensitiveDetectorComponent, replicasOr(detector.Replicas, 1), pod, checksumAnnotation(config, secretVersion))
		return nil
	}); err != nil {
		return err
	}
	return r.reconcilePDB(ctx, m, sensitiveDetectorComponent)
}

func (r *MisskeyReconciler) cleanupSensitiveDetector(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	name := nameSensitiveDetector(m)
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}},
	} {
		if err := r.deleteIfExists(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// 所有ラベルとownerReferenceが一致するSecretのみ削除
func (r *MisskeyReconciler) cleanupGeneratedSensitiveDetectorSecret(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: nameSensitiveDetector(m), Namespace: m.Namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "cloudnative-misskey" || !metav1.IsControlledBy(secret, m) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, secret))
}

func renderSensitiveDetectorConfigSQL(m *misskeyv1beta1.Misskey) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("-- Managed by cloudnative-misskey. Do not edit by hand.\n")
	w("\\set ON_ERROR_STOP on\n")
	if detector := m.Spec.SensitiveDetector; detector != nil {
		w("\\getenv sensitive_detector_mode SENSITIVE_DETECTOR_MODE\n")
		w("\\getenv sensitive_detector_sensitivity SENSITIVE_DETECTOR_SENSITIVITY\n")
		w("\\getenv sensitive_detector_url SENSITIVE_DETECTOR_URL\n")
		w("\\getenv sensitive_detector_key SENSITIVE_DETECTOR_API_KEY\n")
	}
	w("INSERT INTO meta (id) VALUES ('%s') ON CONFLICT (id) DO NOTHING;\n", metaRowID)
	w("UPDATE meta SET\n")
	if detector := m.Spec.SensitiveDetector; detector != nil {
		w("  %q = :'sensitive_detector_mode',\n", "sensitiveMediaDetection")
		w("  %q = :'sensitive_detector_sensitivity',\n", "sensitiveMediaDetectionSensitivity")
		w("  %q = %s,\n", "setSensitiveFlagAutomatically", strconv.FormatBool(detector.SetSensitiveFlagAutomatically))
		w("  %q = %s,\n", "enableSensitiveMediaDetectionForVideos", strconv.FormatBool(detector.EnableForVideos))
		w("  %q = :'sensitive_detector_url',\n", "sensitiveMediaDetectionApiUrl")
		w("  %q = :'sensitive_detector_key',\n", "sensitiveMediaDetectionApiKey")
		w("  %q = %d,\n", "sensitiveMediaDetectionTimeout", int32OrDefault(detector.TimeoutMilliseconds, 60000))
		w("  %q = %d\n", "sensitiveMediaDetectionMaxImagesPerRequest", int32OrDefault(detector.MaxImagesPerRequest, 4))
	} else {
		w("  %q = 'none',\n", "sensitiveMediaDetection")
		w("  %q = 'medium',\n", "sensitiveMediaDetectionSensitivity")
		w("  %q = false,\n", "setSensitiveFlagAutomatically")
		w("  %q = false,\n", "enableSensitiveMediaDetectionForVideos")
		w("  %q = NULL,\n", "sensitiveMediaDetectionApiUrl")
		w("  %q = NULL,\n", "sensitiveMediaDetectionApiKey")
		w("  %q = 60000,\n", "sensitiveMediaDetectionTimeout")
		w("  %q = 4\n", "sensitiveMediaDetectionMaxImagesPerRequest")
	}
	w("WHERE id = '%s';\n", metaRowID)
	return b.String()
}

func sensitiveDetectorConfigJobEnv(m *misskeyv1beta1.Misskey, p plan) []corev1.EnvVar {
	detector := m.Spec.SensitiveDetector
	if detector == nil {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "SENSITIVE_DETECTOR_MODE", Value: detector.Mode},
		{Name: "SENSITIVE_DETECTOR_SENSITIVITY", Value: stringOr(detector.Sensitivity, "medium")},
		{Name: "SENSITIVE_DETECTOR_URL", Value: p.sensitiveDetectorURL},
		secretEnv("SENSITIVE_DETECTOR_API_KEY", p.sensitiveDetectorAPIKeySel),
	}
}

func (r *MisskeyReconciler) sensitiveDetectorConfigHash(ctx context.Context, m *misskeyv1beta1.Misskey, p plan, sql string) (string, error) {
	h := sha256.New()
	write := func(value string) {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	write(sql)
	mp := migratePlan(m, p)
	write(mp.dbHost)
	write(strconv.Itoa(int(mp.dbPort)))
	write(mp.dbName)
	write(mp.dbUser)
	write(mp.dbPassSel.Name)
	write(mp.dbPassSel.Key)
	dbPasswordVersion, err := r.secretVersion(ctx, m.Namespace, mp.dbPassSel.Name)
	if err != nil {
		return "", err
	}
	write(dbPasswordVersion)
	write(strconv.Itoa(len(m.Spec.ImagePullSecrets)))
	for _, secret := range m.Spec.ImagePullSecrets {
		write(secret.Name)
	}
	if detector := m.Spec.SensitiveDetector; detector != nil {
		write(detector.Mode)
		write(stringOr(detector.Sensitivity, "medium"))
		write(p.sensitiveDetectorURL)
		write(p.sensitiveDetectorAPIKeySel.Name)
		write(p.sensitiveDetectorAPIKeySel.Key)
		write(p.sensitiveDetectorConfigImage)
		version, err := r.secretVersion(ctx, m.Namespace, p.sensitiveDetectorAPIKeySel.Name)
		if err != nil {
			return "", err
		}
		write(version)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func buildSensitiveDetectorConfigJob(m *misskeyv1beta1.Misskey, p plan, name string, env []corev1.EnvVar) *batchv1.Job {
	mp := migratePlan(m, p)
	jobEnv := []corev1.EnvVar{
		{Name: "PGHOST", Value: mp.dbHost},
		{Name: "PGPORT", Value: strconv.Itoa(int(mp.dbPort))},
		{Name: "PGDATABASE", Value: mp.dbName},
		{Name: "PGUSER", Value: mp.dbUser},
		secretEnv("PGPASSWORD", mp.dbPassSel),
		{Name: "HOME", Value: "/tmp"},
	}
	jobEnv = append(jobEnv, env...)
	image := p.sensitiveDetectorConfigImage
	if image == "" {
		image = "ghcr.io/cloudnative-pg/postgresql:17"
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace, Labels: labelsFor(m, "sensitive-detector-config")},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(20),
			Parallelism:  int32Ptr(1),
			Completions:  int32Ptr(1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(m, "sensitive-detector-config")},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolPtr(false),
					RestartPolicy:                corev1.RestartPolicyOnFailure,
					ImagePullSecrets:             m.Spec.ImagePullSecrets,
					SecurityContext:              nonRootPodSecurityContext(genericNonRootUID),
					Containers: []corev1.Container{{
						Name:            "sensitive-detector-config",
						Image:           image,
						Command:         []string{"psql", "-1", "-f", "/sql/sensitive-detector.sql"},
						Env:             jobEnv,
						SecurityContext: restrictedContainerSecurityContext(),
						Resources:       resourcesOr(corev1.ResourceRequirements{}, "50m", "64Mi", "128Mi"),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "sql", MountPath: "/sql", ReadOnly: true},
							tmpMount(),
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "sql", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: nameSensitiveDetectorConfig(m)}}}},
						tmpVolume(),
					},
				},
			},
		},
	}
}

func (r *MisskeyReconciler) cleanupSensitiveDetectorConfigJobs(ctx context.Context, m *misskeyv1beta1.Misskey, keep string) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels(selectorFor(m, "sensitive-detector-config"))); err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == keep {
			continue
		}
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *MisskeyReconciler) reconcileSensitiveDetectorConfig(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (bool, error) {
	cm := &corev1.ConfigMap{}
	stateKey := types.NamespacedName{Name: nameSensitiveDetectorConfig(m), Namespace: m.Namespace}
	if m.Spec.SensitiveDetector == nil {
		if err := r.Get(ctx, stateKey, cm); apierrors.IsNotFound(err) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}

	sql := renderSensitiveDetectorConfigSQL(m)
	hash, err := r.sensitiveDetectorConfigHash(ctx, m, p, sql)
	if err != nil {
		return false, err
	}
	jobName := nameSensitiveDetectorConfigJob(m, hash)
	if err := r.cleanupSensitiveDetectorConfigJobs(ctx, m, jobName); err != nil {
		return false, err
	}

	cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameSensitiveDetectorConfig(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, cm, func() error {
		cm.Labels = labelsFor(m, "sensitive-detector-config")
		cm.Data = map[string]string{
			"sensitive-detector.sql": sql,
			"desired-hash":           hash,
			"enabled":                strconv.FormatBool(m.Spec.SensitiveDetector != nil),
		}
		return nil
	}); err != nil {
		return false, err
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: m.Namespace}, job)
	if apierrors.IsNotFound(err) {
		job = buildSensitiveDetectorConfigJob(m, p, jobName, sensitiveDetectorConfigJobEnv(m, p))
		if err := controllerutil.SetControllerReference(m, job, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, job); err != nil {
			return false, err
		}
		r.event(m, corev1.EventTypeNormal, "SensitiveDetectorConfiguring", "Configure", "created Sensitive Detector meta Job %s", job.Name)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if job.Status.Succeeded == 0 && job.Status.Failed >= 1 {
		r.event(m, corev1.EventTypeWarning, "SensitiveDetectorConfigFailed", "Configure", "Sensitive Detector meta Job %s failed (%d); delete the Job to retry", job.Name, job.Status.Failed)
	}
	return job.Status.Succeeded >= 1, nil
}

func (r *MisskeyReconciler) cleanupSensitiveDetectorConfigState(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	if err := r.cleanupSensitiveDetectorConfigJobs(ctx, m, ""); err != nil {
		return err
	}
	return r.deleteIfExists(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameSensitiveDetectorConfig(m), Namespace: m.Namespace}})
}

func (r *MisskeyReconciler) hasSensitiveDetectorConfigState(ctx context.Context, m *misskeyv1beta1.Misskey) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: nameSensitiveDetectorConfig(m), Namespace: m.Namespace}, cm)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (r *MisskeyReconciler) misskeyWorkloadsConverged(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) bool {
	checksum, err := r.misskeyChecksum(ctx, m, p)
	if err != nil {
		return false
	}
	want := checksum[configChecksumAnnotation]
	for _, name := range []string{nameApp(m), nameWorker(m)} {
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, dep); err != nil {
			return false
		}
		if dep.Spec.Template.Annotations[configChecksumAnnotation] != want || dep.Status.ObservedGeneration < dep.Generation {
			return false
		}
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		if dep.Status.UpdatedReplicas < desired || dep.Status.AvailableReplicas < desired {
			return false
		}
	}
	return true
}

// 新設定の反映完了まで旧リソースを保持
func (r *MisskeyReconciler) finalizeSensitiveDetectorCleanup(ctx context.Context, m *misskeyv1beta1.Misskey, p plan, configComplete bool) error {
	if !configComplete || !r.misskeyWorkloadsConverged(ctx, m, p) {
		return nil
	}
	if p.sensitiveDetectorManaged {
		detector := m.Spec.SensitiveDetector
		if detector == nil || detector.APIKeySecret == nil || detector.APIKeySecret.Name == nameSensitiveDetector(m) {
			return nil
		}
		converged, err := r.sensitiveDetectorConverged(ctx, m)
		if err != nil || !converged {
			return err
		}
		return r.cleanupGeneratedSensitiveDetectorSecret(ctx, m)
	}
	if err := r.cleanupSensitiveDetector(ctx, m); err != nil {
		return err
	}
	if err := r.cleanupGeneratedSensitiveDetectorSecret(ctx, m); err != nil {
		return err
	}
	if !p.sensitiveDetectorEnabled {
		return r.cleanupSensitiveDetectorConfigState(ctx, m)
	}
	return nil
}

func (r *MisskeyReconciler) sensitiveDetectorConverged(ctx context.Context, m *misskeyv1beta1.Misskey) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameSensitiveDetector(m), Namespace: m.Namespace}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	return dep.Status.ObservedGeneration >= dep.Generation && dep.Status.UpdatedReplicas >= desired && dep.Status.AvailableReplicas >= desired, nil
}

func (r *MisskeyReconciler) sensitiveDetectorConfigComplete(ctx context.Context, m *misskeyv1beta1.Misskey) bool {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameSensitiveDetectorConfig(m), Namespace: m.Namespace}, cm); err != nil {
		return false
	}
	hash := cm.Data["desired-hash"]
	if len(hash) < 10 {
		return false
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: nameSensitiveDetectorConfigJob(m, hash), Namespace: m.Namespace}, job); err != nil {
		return false
	}
	return job.Status.Succeeded >= 1
}

func (r *MisskeyReconciler) sensitiveDetectorCondition(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (metav1.ConditionStatus, string, string) {
	if !p.sensitiveDetectorEnabled {
		return metav1.ConditionFalse, "Disabling", "Sensitive Detector cleanup is in progress"
	}
	if !r.sensitiveDetectorConfigComplete(ctx, m) {
		return metav1.ConditionFalse, "Configuring", "waiting for Sensitive Detector meta configuration"
	}
	if !p.sensitiveDetectorManaged {
		return metav1.ConditionTrue, "Configured", "external Sensitive Detector configured"
	}
	status, reason, message := r.deploymentReady(ctx, m, nameSensitiveDetector(m))
	return status, reason, message
}
