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
	"crypto/rand"
	"encoding/hex"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// randomHex returns a cryptographically-random hex string of n bytes.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// reconcileMeiliSecret ensures the operator-managed master key Secret exists.
// It is created only when MeiliSearch is managed and the user did not supply
// their own master key. The generated key is never overwritten once set.
func (r *MisskeyReconciler) reconcileMeiliSecret(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	if m.Spec.Search.Meilisearch.MasterKeySecret != nil {
		return nil // user manages the key
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameMeili(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, "meilisearch")
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data[meiliMasterKeyID]; !ok {
			key, err := randomHex(32)
			if err != nil {
				return err
			}
			secret.Data[meiliMasterKeyID] = []byte(key)
		}
		return nil
	})
}

// reconcileMeilisearch creates/updates the MeiliSearch Service and StatefulSet.
func (r *MisskeyReconciler) reconcileMeilisearch(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
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

	image := stringOr(m.Spec.Search.Meilisearch.Image, "getmeili/meilisearch:v1.11")
	storage := quantityOr(m.Spec.Search.Meilisearch.Storage, "10Gi")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nameMeili(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, "meilisearch")
		sts.Spec.ServiceName = nameMeili(m)
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, "meilisearch")}
		sts.Spec.Template.ObjectMeta.Labels = labelsFor(m, "meilisearch")
		sts.Spec.Template.Spec = corev1.PodSpec{
			SecurityContext: nonRootPodSecurityContext(genericNonRootUID),
			Containers: []corev1.Container{
				{
					Name:            "meilisearch",
					Image:           image,
					SecurityContext: restrictedContainerSecurityContext(),
					Resources:       resourcesOr(m.Spec.Search.Meilisearch.Resources, "100m", "256Mi", "1Gi"),
					Env: []corev1.EnvVar{
						secretEnv("MEILI_MASTER_KEY", p.meiliKeySel),
						{Name: "MEILI_ENV", Value: "production"},
						{Name: "MEILI_NO_ANALYTICS", Value: "true"},
						{Name: "MEILI_DB_PATH", Value: "/meili_data"},
					},
					Ports: []corev1.ContainerPort{{ContainerPort: meiliPort}},
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
					// subPath keeps the data out of the volume root, where an ext4
					// lost+found dir would make MeiliSearch fail to infer its DB version.
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/meili_data", SubPath: "data"}},
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
