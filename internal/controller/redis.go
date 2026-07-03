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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// 公式redisイメージが動作するuid
const redisUID = 999

// managed RedisのServiceとStatefulSetを作成/更新
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
	policy := stringOr(m.Spec.Redis.MaxMemoryPolicy, "noeviction")
	storage := quantityOr(m.Spec.Redis.Storage, "2Gi")

	// キュー(BullMQ)耐久化のためAOFを既定有効
	args := []string{"redis-server", "--maxmemory", maxMem, "--maxmemory-policy", policy}
	if boolOr(m.Spec.Redis.AppendOnly, true) {
		args = append(args, "--appendonly", "yes")
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nameRedis(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, "redis")
		sts.Spec.ServiceName = nameRedis(m)
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, "redis")}
		sts.Spec.Template.ObjectMeta.Labels = labelsFor(m, "redis")
		sts.Spec.Template.Spec = corev1.PodSpec{
			SecurityContext: nonRootPodSecurityContext(redisUID),
			Containers: []corev1.Container{
				{
					Name:            "redis",
					Image:           image,
					Args:            args,
					SecurityContext: restrictedContainerSecurityContext(),
					Resources:       resourcesOr(m.Spec.Redis.Resources, "50m", "128Mi", "512Mi"),
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
	}); err != nil {
		return err
	}
	return r.reconcileRedisNetworkPolicy(ctx, m)
}

// managed Redisへのingressをapp/workerに限るNetworkPolicyを作成/更新
// CNIが強制する場合のみ有効
func (r *MisskeyReconciler) reconcileRedisNetworkPolicy(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	if !boolOr(m.Spec.Redis.NetworkPolicy, true) {
		return nil
	}
	port := intstr.FromInt32(redisPort)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: nameRedis(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, np, func() error {
		np.Labels = labelsFor(m, "redis")
		np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: selectorFor(m, "redis")}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{MatchLabels: selectorFor(m, roleApp)}},
				{PodSelector: &metav1.LabelSelector{MatchLabels: selectorFor(m, roleWorker)}},
			},
			Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
		}}
		return nil
	})
}
