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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// 公式redisイメージが動作するuid(standalone)
const redisUID = 999

// redisManagedInstance: 1つのmanaged redisインスタンスのprovisioning設定
type redisManagedInstance struct {
	suffix string // ""=default、role時は"jobqueue"等
	ha     bool

	// standalone用
	image           string
	maxMemory       string
	maxMemoryPolicy string
	appendOnly      bool
	storage         resource.Quantity
	storageClass    *string
	resources       corev1.ResourceRequirements
	networkPolicy   bool

	// HA用
	replicas        int32
	sentinels       int32
	haImage         string
	haSentinelImage string
}

// allRedisSuffixes: 取り得る全suffix(default + 4role)。cleanup走査用
func allRedisSuffixes() []string {
	out := []string{""}
	for _, rd := range redisRoleDescs {
		out = append(out, rd.nameSuffix)
	}
	return out
}

// managedRedisInstances: spec.redisから望ましいmanagedインスタンスを導出(external roleは除外)
func managedRedisInstances(m *misskeyv1alpha1.Misskey) []redisManagedInstance {
	rs := m.Spec.Redis
	var out []redisManagedInstance
	if rs.External == nil {
		out = append(out, buildManagedInstance(m, "", nil, rs.HA))
	}
	if rs.Roles != nil {
		for _, rd := range redisRoleDescs {
			role := rd.get(rs.Roles)
			if role == nil || role.External != nil {
				continue
			}
			ha := role.HA
			if ha == nil {
				ha = rs.HA // role未指定はdefaultのHAを継承
			}
			out = append(out, buildManagedInstance(m, rd.nameSuffix, role, ha))
		}
	}
	return out
}

// buildManagedInstance: default設定 + role override + HA解決
func buildManagedInstance(m *misskeyv1alpha1.Misskey, suffix string, role *misskeyv1alpha1.RedisRole, ha *misskeyv1alpha1.RedisHA) redisManagedInstance {
	rs := m.Spec.Redis
	inst := redisManagedInstance{
		suffix:          suffix,
		image:           stringOr(rs.Image, "redis:8-alpine"),
		maxMemory:       stringOr(rs.MaxMemory, "400mb"),
		maxMemoryPolicy: stringOr(rs.MaxMemoryPolicy, "noeviction"),
		appendOnly:      boolOr(rs.AppendOnly, true),
		storage:         quantityOr(rs.Storage, "2Gi"),
		storageClass:    rs.StorageClassName,
		resources:       rs.Resources,
		networkPolicy:   boolOr(rs.NetworkPolicy, true),
	}
	if role != nil {
		inst.maxMemory = stringOr(role.MaxMemory, inst.maxMemory)
		inst.maxMemoryPolicy = stringOr(role.MaxMemoryPolicy, inst.maxMemoryPolicy)
		if !role.Storage.IsZero() {
			inst.storage = role.Storage
		}
		if role.StorageClassName != nil {
			inst.storageClass = role.StorageClassName
		}
		if len(role.Resources.Requests) > 0 || len(role.Resources.Limits) > 0 {
			inst.resources = role.Resources
		}
	}
	if ha != nil && boolOr(ha.Enabled, true) {
		inst.ha = true
		inst.replicas = int32OrDefault(ha.Replicas, 3)
		inst.sentinels = int32OrDefault(ha.Sentinels, 3)
		inst.haImage = stringOr(ha.Image, "quay.io/opstree/redis:v8.2.5")
		inst.haSentinelImage = stringOr(ha.SentinelImage, "quay.io/opstree/redis-sentinel:v8.2.5")
	}
	return inst
}

// reconcileRedis: 望ましいmanagedインスタンス(default+managed roles)を各々standalone/HAで用意し、
// 望ましくなくなった(external化/role削除/mode切替)インスタンスを掃除
func (r *MisskeyReconciler) reconcileRedis(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	desired := managedRedisInstances(m)
	seenStandalone := map[string]bool{}
	seenHA := map[string]bool{}
	for _, inst := range desired {
		if inst.ha {
			// standalone→HA切替時の名前衝突回避のため先に掃除
			if err := r.deleteRedisStandalone(ctx, m, inst.suffix); err != nil {
				return err
			}
			if err := r.reconcileRedisHA(ctx, m, inst); err != nil {
				return err
			}
			seenHA[inst.suffix] = true
		} else {
			if err := r.deleteRedisHA(ctx, m, inst.suffix); err != nil {
				return err
			}
			if err := r.reconcileRedisStandalone(ctx, m, inst); err != nil {
				return err
			}
			seenStandalone[inst.suffix] = true
		}
	}
	// 望ましくないインスタンスを全suffixで掃除(PVCはデータ保護のため残す)
	for _, suffix := range allRedisSuffixes() {
		if !seenStandalone[suffix] {
			if err := r.deleteRedisStandalone(ctx, m, suffix); err != nil {
				return err
			}
		}
		if !seenHA[suffix] {
			if err := r.deleteRedisHA(ctx, m, suffix); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileRedisStandalone: 単一pod Redis(Service+StatefulSet+任意NetworkPolicy)
func (r *MisskeyReconciler) reconcileRedisStandalone(ctx context.Context, m *misskeyv1alpha1.Misskey, inst redisManagedInstance) error {
	name := nameRedisInstance(m, inst.suffix)
	comp := redisComponent(inst.suffix)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, comp)
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = selectorFor(m, comp)
		svc.Spec.Ports = []corev1.ServicePort{{Port: redisPort}}
		return nil
	}); err != nil {
		return err
	}

	// キュー耐久化のためAOFを既定有効
	args := []string{"redis-server", "--maxmemory", inst.maxMemory, "--maxmemory-policy", inst.maxMemoryPolicy}
	if inst.appendOnly {
		args = append(args, "--appendonly", "yes")
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, comp)
		sts.Spec.ServiceName = name
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, comp)}
		sts.Spec.Template.ObjectMeta.Labels = labelsFor(m, comp)
		sts.Spec.Template.Spec = corev1.PodSpec{
			SecurityContext: nonRootPodSecurityContext(redisUID),
			Containers: []corev1.Container{
				{
					Name:            "redis",
					Image:           inst.image,
					Args:            args,
					SecurityContext: restrictedContainerSecurityContext(),
					Resources:       resourcesOr(inst.resources, "50m", "128Mi", "512Mi"),
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
					StorageClassName: inst.storageClass,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: inst.storage},
					},
				},
			},
		}
		return nil
	}); err != nil {
		return err
	}
	return r.reconcileRedisStandaloneNetworkPolicy(ctx, m, inst)
}

// reconcileRedisStandaloneNetworkPolicy: standalone Redisへのingressをapp/workerに限る
// CNIが強制する場合のみ有効, spec.redis.networkPolicy=falseで無効化
func (r *MisskeyReconciler) reconcileRedisStandaloneNetworkPolicy(ctx context.Context, m *misskeyv1alpha1.Misskey, inst redisManagedInstance) error {
	name := nameRedisInstance(m, inst.suffix)
	comp := redisComponent(inst.suffix)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if !inst.networkPolicy {
		return r.deleteIfExists(ctx, np)
	}
	port := intstr.FromInt32(redisPort)
	return r.apply(ctx, m, np, func() error {
		np.Labels = labelsFor(m, comp)
		np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: selectorFor(m, comp)}
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

// deleteRedisStandalone: 指定suffixのstandalone資源(STS/Service/NetworkPolicy)を掃除
func (r *MisskeyReconciler) deleteRedisStandalone(ctx context.Context, m *misskeyv1alpha1.Misskey, suffix string) error {
	name := nameRedisInstance(m, suffix)
	ns := m.Namespace
	objs := []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
	}
	for _, o := range objs {
		if err := r.deleteIfExists(ctx, o); err != nil {
			return err
		}
	}
	return nil
}
