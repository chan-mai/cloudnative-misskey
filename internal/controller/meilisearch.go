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
	"crypto/rand"
	"encoding/hex"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// nバイトの暗号学的乱数hex文字列を返す
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// operator管理のマスターキーSecretの存在を保証
// MeiliSearchがmanagedかつユーザがマスターキー未指定の時のみ作成。生成後のキーは上書きしない
func (r *MisskeyReconciler) reconcileMeiliSecret(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	if m.Spec.Search.Meilisearch.MasterKeySecret != nil {
		return nil // キーはユーザが管理
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameMeili(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, "meilisearch")
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data[meiliMasterKeyID]; !ok || rotationRequested(m, secret) {
			key, err := randomHex(32)
			if err != nil {
				return err
			}
			secret.Data[meiliMasterKeyID] = []byte(key)
		}
		markRotation(m, secret)
		return nil
	})
}

// MeiliSearchのServiceとStatefulSetを作成/更新
func (r *MisskeyReconciler) reconcileMeilisearch(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameMeili(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, "meilisearch")
		svc.Spec.Selector = selectorFor(m, "meilisearch")
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       meiliPort,
			TargetPort: intstr.FromInt32(meiliPort),
		}}
		return nil
	}); err != nil {
		return err
	}

	image := stringOr(m.Spec.Search.Meilisearch.Image, "getmeili/meilisearch:v1")
	storage := quantityOr(m.Spec.Search.Meilisearch.Storage, "10Gi")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nameMeili(m), Namespace: m.Namespace}}
	return r.applyStatefulSet(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, "meilisearch")
		sts.Spec.ServiceName = nameMeili(m)
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, "meilisearch")}
		sts.Spec.Template.Labels = labelsFor(m, "meilisearch")
		// master keyのローテーション(値変化=resourceVersion変化)でpodをrollし新keyを取り込む
		ver, err := r.secretVersion(ctx, m.Namespace, p.meiliKeySel.Name)
		if err != nil {
			return err
		}
		sts.Spec.Template.Annotations = checksumAnnotation(ver)
		meiliEnv := []corev1.EnvVar{
			secretEnv("MEILI_MASTER_KEY", p.meiliKeySel),
			{Name: "MEILI_ENV", Value: "production"},
			{Name: "MEILI_NO_ANALYTICS", Value: "true"},
			{Name: "MEILI_DB_PATH", Value: "/meili_data"},
		}
		// monitoring時のみ/metricsエンドポイントを有効化
		if monitoringEnabled(m) {
			meiliEnv = append(meiliEnv, corev1.EnvVar{Name: "MEILI_EXPERIMENTAL_ENABLE_METRICS", Value: "true"})
		}
		sts.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext:              nonRootPodSecurityContext(genericNonRootUID),
			Volumes:                      []corev1.Volume{tmpVolume()},
			Containers: []corev1.Container{
				{
					Name:            "meilisearch",
					Image:           image,
					SecurityContext: restrictedContainerSecurityContext(),
					Resources:       resourcesOr(m.Spec.Search.Meilisearch.Resources, "100m", "256Mi", "1Gi"),
					Env:             meiliEnv,
					Ports:           []corev1.ContainerPort{{ContainerPort: meiliPort}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(meiliPort)},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       10,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(meiliPort)},
						},
						InitialDelaySeconds: 15,
						PeriodSeconds:       20,
					},
					// subPathでデータをvolumeルート外に置く
					// volumeルートにext4のlost+foundがあるとMeiliSearchがDBバージョンを推定できず失敗するため
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/meili_data", SubPath: "data"}, tmpMount()},
				},
			},
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: m.Spec.Search.Meilisearch.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
					},
				},
			},
		}
		return nil
	})
}
