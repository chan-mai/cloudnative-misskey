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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// OT-CONTAINER-KIT redis-operatorのGVK。opstree Redis8イメージでSentinel HAを構成
var (
	redisReplicationGVK = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisReplication"}
	redisSentinelGVK    = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisSentinel"}
)

// opstree redisイメージはnon-root。/data書込にfsGroupが要る(未設定だとAOF/PIDでcrashloop)
const opstreeRedisUID = 1000

// reconcileRedisHA: 1インスタンス分のRedisReplication+RedisSentinelをSSA
func (r *MisskeyReconciler) reconcileRedisHA(ctx context.Context, m *misskeyv1alpha1.Misskey, inst redisManagedInstance) error {
	if err := r.applySSA(ctx, m, buildRedisReplication(m, inst)); err != nil {
		return err
	}
	return r.applySSA(ctx, m, buildRedisSentinel(m, inst))
}

// buildRedisReplication: primary+replicaのRedisReplication unstructured
func buildRedisReplication(m *misskeyv1alpha1.Misskey, inst redisManagedInstance) *unstructured.Unstructured {
	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(ptrResources(resourcesOr(inst.resources, "50m", "128Mi", "512Mi")))
	spec := map[string]any{
		"clusterSize": int64(inst.replicas),
		"kubernetesConfig": map[string]any{
			"image":           inst.haImage,
			"imagePullPolicy": "IfNotPresent",
			"resources":       res,
		},
		// opstreeイメージのnon-root書込にfsGroup必須
		"podSecurityContext": redisHAPodSecurityContext(),
		"storage": map[string]any{
			"volumeClaimTemplate": map[string]any{
				"spec": redisPVCSpec(inst),
			},
		},
	}
	return redisUnstructured(m, inst.suffix, redisReplicationGVK, nameRedisInstance(m, inst.suffix), spec)
}

// buildRedisSentinel: RedisReplicationを監視するRedisSentinel unstructured
func buildRedisSentinel(m *misskeyv1alpha1.Misskey, inst redisManagedInstance) *unstructured.Unstructured {
	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(ptrResources(resourcesOr(corev1.ResourceRequirements{}, "25m", "64Mi", "128Mi")))
	quorum := strconv.Itoa(int(inst.sentinels)/2 + 1)
	spec := map[string]any{
		"clusterSize": int64(inst.sentinels),
		"kubernetesConfig": map[string]any{
			"image":           inst.haSentinelImage,
			"imagePullPolicy": "IfNotPresent",
			"resources":       res,
		},
		"podSecurityContext": redisHAPodSecurityContext(),
		"redisSentinelConfig": map[string]any{
			"redisReplicationName": nameRedisInstance(m, inst.suffix),
			"masterGroupName":      redisMasterGroup,
			"quorum":               quorum,
		},
	}
	return redisUnstructured(m, inst.suffix, redisSentinelGVK, nameRedisInstance(m, inst.suffix), spec)
}

// redisUnstructured: 共通のGVK/name/namespace/labels/specを持つunstructuredを組む
// OT operatorはCRのlabelをpodへ継承するため、instance/component labelは付けない
// (付けるとisolation NPがoperator管理podを選択しredis-operatorのcross-ns疎通を遮断する)
// postgres(CNPG)を除外するのと同じ思想。app/worker→pod疎通はredisEgressRuleで許可
func redisUnstructured(m *misskeyv1alpha1.Misskey, suffix string, gvk schema.GroupVersionKind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(m.Namespace)
	// tenant集計/observability用の安全なlabelのみ(instance/component無し)
	u.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by":   "cloud-native-misskey",
		"cloudnative-misskey.dev/tenant": tenantOf(m),
	})
	u.Object["spec"] = spec
	return u
}

// redisHAPodSecurityContext: opstreeイメージ用のfsGroup付きpod securityContext
func redisHAPodSecurityContext() map[string]any {
	return map[string]any{
		"runAsUser":  int64(opstreeRedisUID),
		"runAsGroup": int64(opstreeRedisUID),
		"fsGroup":    int64(opstreeRedisUID),
	}
}

// redisPVCSpec: HA各podのPVC template spec
func redisPVCSpec(inst redisManagedInstance) map[string]any {
	spec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": inst.storage.String()}},
	}
	if inst.storageClass != nil && *inst.storageClass != "" {
		spec["storageClassName"] = *inst.storageClass
	}
	return spec
}

// ptrResources: ToUnstructured用にポインタ化
func ptrResources(r corev1.ResourceRequirements) *corev1.ResourceRequirements { return &r }

// deleteRedisHA: 指定suffixのRedisReplication/RedisSentinelを掃除。PVCは残す
func (r *MisskeyReconciler) deleteRedisHA(ctx context.Context, m *misskeyv1alpha1.Misskey, suffix string) error {
	name := nameRedisInstance(m, suffix)
	for _, gvk := range []schema.GroupVersionKind{redisReplicationGVK, redisSentinelGVK} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetNamespace(m.Namespace)
		if err := r.deleteIfExists(ctx, u); err != nil {
			return err
		}
	}
	return nil
}
