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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// redisUID is the uid the official redis image runs as.
const redisUID = 999

// reconcileRedis creates/updates the managed Redis Service and StatefulSet.
func (r *MisskeyReconciler) reconcileRedis(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameRedis(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, "redis")
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = selectorFor(m, "redis")
		svc.Spec.Ports = []corev1.ServicePort{{Port: redisPort}}
		return nil
	}); err != nil {
		return err
	}

	image := stringOr(m.Spec.Redis.Image, "redis:7-alpine")
	maxMem := stringOr(m.Spec.Redis.MaxMemory, "400mb")
	storage := quantityOr(m.Spec.Redis.Storage, "2Gi")

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nameRedis(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, "redis")
		sts.Spec.ServiceName = nameRedis(m)
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, "redis")}
		sts.Spec.Template.ObjectMeta.Labels = labelsFor(m, "redis")
		sts.Spec.Template.Spec = corev1.PodSpec{
			SecurityContext: nonRootPodSecurityContext(redisUID),
			Containers: []corev1.Container{
				{
					Name:  "redis",
					Image: image,
					Args: []string{
						"redis-server",
						"--maxmemory", maxMem,
						"--maxmemory-policy", "allkeys-lru",
					},
					SecurityContext: restrictedContainerSecurityContext(),
					Resources:       m.Spec.Redis.Resources,
					Ports:           []corev1.ContainerPort{{ContainerPort: redisPort}},
					VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
				},
			},
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: m.Spec.Redis.StorageClassName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
					},
				},
			},
		}
		return nil
	})
}
