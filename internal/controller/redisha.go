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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// redisAuthSecretRef: OT operator redisSecret/EnvVarSource用のsecret参照
func redisAuthSecretRef(m *misskeyv1beta1.Misskey) map[string]any {
	return map[string]any{"name": nameRedisAuthSecret(m), "key": "password"}
}

// redisAuthSecretKeySelector: requirepass secretのSecretKeySelector(env注入・config置換用)
func redisAuthSecretKeySelector(m *misskeyv1beta1.Misskey) corev1.SecretKeySelector {
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: nameRedisAuthSecret(m)}, Key: "password"}
}

// reconcileRedisAuthSecret: managed redisのrequirepass用にrandom passwordのSecretを保証(冪等・生成後は不変)
func (r *MisskeyReconciler) reconcileRedisAuthSecret(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameRedisAuthSecret(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, "redis")
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data["password"]; !ok || rotationRequested(m, secret) {
			pw, err := randomHex(24)
			if err != nil {
				return err
			}
			secret.Data["password"] = []byte(pw)
		}
		markRotation(m, secret)
		return nil
	})
}

// OT-CONTAINER-KIT redis-operatorのGVK。opstree Redis8イメージでSentinel HAを構成
var (
	redisReplicationGVK = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisReplication"}
	redisSentinelGVK    = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisSentinel"}
)

// opstree redisイメージはnon-root。/data書込にfsGroupが要る(未設定だとAOF/PIDでcrashloop)
const opstreeRedisUID = 1000

// reconcileRedisHA: 1インスタンス分のRedisReplication+RedisSentinelをSSA
func (r *MisskeyReconciler) reconcileRedisHA(ctx context.Context, m *misskeyv1beta1.Misskey, inst redisManagedInstance) error {
	if err := r.applySSA(ctx, m, buildRedisReplication(m, inst)); err != nil {
		return err
	}
	return r.applySSA(ctx, m, buildRedisSentinel(m, inst))
}

// buildRedisReplication: primary+replicaのRedisReplication unstructured
func buildRedisReplication(m *misskeyv1beta1.Misskey, inst redisManagedInstance) *unstructured.Unstructured {
	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(ptrResources(resourcesOr(inst.resources, "50m", "128Mi", "512Mi")))
	spec := map[string]any{
		"clusterSize": int64(inst.replicas),
		"kubernetesConfig": map[string]any{
			"image":           inst.haImage,
			"imagePullPolicy": "IfNotPresent",
			"resources":       res,
			// requirepass認証(operator生成secret)。任意podからの無認証read/writeを防ぐ
			"redisSecret": redisAuthSecretRef(m),
			// HA→standalone/role削除時にデータPVCを消さない(scale/delete両方Retain)
			"persistentVolumeClaimRetentionPolicy": map[string]any{
				"whenDeleted": "Retain",
				"whenScaled":  "Retain",
			},
		},
		// opstreeイメージのnon-root書込にfsGroup必須
		"podSecurityContext": redisHAPodSecurityContext(),
		"storage": map[string]any{
			"volumeClaimTemplate": map[string]any{
				"spec": redisPVCSpec(inst),
			},
		},
	}
	// monitoring時はOTのredis_exporter sidecar(auth自動配線)を有効化
	if monitoringEnabled(m) {
		spec["redisExporter"] = map[string]any{"enabled": true}
	}
	return redisUnstructured(m, inst.suffix, redisReplicationGVK, nameRedisHA(m, inst.suffix), spec)
}

// buildRedisSentinel: RedisReplicationを監視するRedisSentinel unstructured
func buildRedisSentinel(m *misskeyv1beta1.Misskey, inst redisManagedInstance) *unstructured.Unstructured {
	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(ptrResources(resourcesOr(corev1.ResourceRequirements{}, "25m", "64Mi", "128Mi")))
	quorum := strconv.Itoa(int(inst.sentinels)/2 + 1)
	spec := map[string]any{
		"clusterSize": int64(inst.sentinels),
		"kubernetesConfig": map[string]any{
			"image":           inst.haSentinelImage,
			"imagePullPolicy": "IfNotPresent",
			"resources":       res,
			"redisSecret":     redisAuthSecretRef(m),
		},
		"podSecurityContext": redisHAPodSecurityContext(),
		"redisSentinelConfig": map[string]any{
			"redisReplicationName": nameRedisHA(m, inst.suffix),
			"masterGroupName":      redisMasterGroup,
			"quorum":               quorum,
			// sentinelがauth付きmasterを監視するための認証情報
			"redisReplicationPassword": map[string]any{
				"secretKeyRef": map[string]any{"name": nameRedisAuthSecret(m), "key": "password"},
			},
		},
	}
	return redisUnstructured(m, inst.suffix, redisSentinelGVK, nameRedisHA(m, inst.suffix), spec)
}

// redisUnstructured: 共通のGVK/name/namespace/labels/specを持つunstructuredを組む
// OT operatorはCRのlabelをpodへ継承するため、instance/component labelは付けない
// (付けるとisolation NPがoperator管理podを選択しredis-operatorのcross-ns疎通を遮断する)
// postgres(CNPG)を除外するのと同じ思想。app/worker→pod疎通はredisEgressRuleで許可
func redisUnstructured(m *misskeyv1beta1.Misskey, suffix string, gvk schema.GroupVersionKind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(m.Namespace)
	// tenant集計/observability用の安全なlabelのみ(instance/component無し)
	u.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by":   "cloudnative-misskey",
		"cloudnative-misskey.dev/tenant": tenantOf(m),
	})
	u.Object["spec"] = spec
	return u
}

// redisHAPodSecurityContext: opstreeイメージ用のpod securityContext
// standalone(nonRootPodSecurityContext)と同等にrunAsNonRoot+seccomp RuntimeDefaultを強制
func redisHAPodSecurityContext() map[string]any {
	return map[string]any{
		"runAsNonRoot": true,
		"runAsUser":    int64(opstreeRedisUID),
		"runAsGroup":   int64(opstreeRedisUID),
		"fsGroup":      int64(opstreeRedisUID),
		"seccompProfile": map[string]any{
			"type": "RuntimeDefault",
		},
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
func (r *MisskeyReconciler) deleteRedisHA(ctx context.Context, m *misskeyv1beta1.Misskey, suffix string) error {
	name := nameRedisHA(m, suffix)
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
