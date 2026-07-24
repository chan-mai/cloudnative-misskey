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

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// 公式redisイメージが動作するuid(standalone)
const redisUID = 999

// redisPingProbe: standalone redisのping probe。redis-cliはREDISCLI_AUTH envで自動AUTH
func redisPingProbe(period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"redis-cli", "ping"}}},
		PeriodSeconds:  period,
		TimeoutSeconds: 3,
	}
}

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
func managedRedisInstances(m *misskeyv1beta1.Misskey) []redisManagedInstance {
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
			// role単位で独立(ポインタ存在=有効)。role.ha無しはstandalone、redis.haの継承はしない
			out = append(out, buildManagedInstance(m, rd.nameSuffix, role, role.HA))
		}
	}
	return out
}

// buildManagedInstance: default設定 + role override + HA解決
func buildManagedInstance(m *misskeyv1beta1.Misskey, suffix string, role *misskeyv1beta1.RedisRole, ha *misskeyv1beta1.RedisHA) redisManagedInstance {
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
	if ha != nil {
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
func (r *MisskeyReconciler) reconcileRedis(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	desired := managedRedisInstances(m)
	// managedが1つでもあればrequirepass用のauth secretを用意(standalone/HA共通, CR/pod参照前に存在させる)
	if len(desired) > 0 {
		if err := r.reconcileRedisAuthSecret(ctx, m); err != nil {
			return err
		}
	}
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
			if err := r.reconcileRedisHANetworkPolicy(ctx, m, inst); err != nil {
				return err
			}
			seenHA[inst.suffix] = true
		} else {
			if err := r.deleteRedisHA(ctx, m, inst.suffix); err != nil {
				return err
			}
			if err := r.deleteRedisHANetworkPolicy(ctx, m, inst.suffix); err != nil {
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
			if err := r.deleteRedisHANetworkPolicy(ctx, m, suffix); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileRedisHANetworkPolicy: operator管理HA redis/sentinel podへのingressを制限
// app/worker + intra-HA + allowedNamespaces(redis-operator/keda)のみ許可
// HA podはinstance labelを持たずnetwork.isolationのNPに乗らないため、その穴を埋める専用NP
// network.isolation.enabledでgate(network.isolationの一部として機能)。requirepassと併せた多層防御
func (r *MisskeyReconciler) reconcileRedisHANetworkPolicy(ctx context.Context, m *misskeyv1beta1.Misskey, inst redisManagedInstance) error {
	name := nameRedisInstance(m, inst.suffix) + "-ha"
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if !boolOr(m.Spec.Network.Isolation.Enabled, true) {
		return r.deleteIfExists(ctx, np)
	}
	rp := intstr.FromInt32(redisPort)
	sp := intstr.FromInt32(sentinelPort)
	// operatorはCR名をpodのappラベルに付ける(replication=<name>、sentinel=<name>-sentinel)
	haApps := []string{nameRedisHA(m, inst.suffix), nameRedisSentinelService(m, inst.suffix)}
	appIn := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "app", Operator: metav1.LabelSelectorOpIn, Values: haApps,
	}}}
	return r.apply(ctx, m, np, func() error {
		np.Labels = labelsFor(m, "redis")
		np.Spec.PodSelector = appIn
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		from := []networkingv1.NetworkPolicyPeer{
			{PodSelector: &metav1.LabelSelector{MatchLabels: selectorFor(m, roleApp)}},
			{PodSelector: &metav1.LabelSelector{MatchLabels: selectorFor(m, roleWorker)}},
			// replication/sentinel相互(intra-HA)
			{PodSelector: &appIn},
		}
		// redis-operator/KEDA等のcross-ns(管理・metric取得)。network.isolation.allowedNamespacesを流用
		for _, ns := range m.Spec.Network.Isolation.AllowedNamespaces {
			from = append(from, networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nsNameLabel: ns}},
			})
		}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From:  from,
			Ports: []networkingv1.NetworkPolicyPort{{Port: &rp}, {Port: &sp}},
		}}
		return nil
	})
}

// deleteRedisHANetworkPolicy: 指定suffixのHA redis NPを掃除
func (r *MisskeyReconciler) deleteRedisHANetworkPolicy(ctx context.Context, m *misskeyv1beta1.Misskey, suffix string) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: nameRedisInstance(m, suffix) + "-ha", Namespace: m.Namespace}}
	return r.deleteIfExists(ctx, np)
}

// reconcileRedisStandalone: 単一pod Redis(Service+StatefulSet+任意NetworkPolicy)
func (r *MisskeyReconciler) reconcileRedisStandalone(ctx context.Context, m *misskeyv1beta1.Misskey, inst redisManagedInstance) error {
	name := nameRedisInstance(m, inst.suffix)
	comp := redisComponent(inst.suffix)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, comp)
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = selectorFor(m, comp)
		svc.Spec.Ports = []corev1.ServicePort{{Name: "redis", Port: redisPort}}
		if monitoringEnabled(m) {
			svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "metrics", Port: redisExporterPort, TargetPort: intstr.FromInt32(redisExporterPort)})
		}
		return nil
	}); err != nil {
		return err
	}

	// requirepass認証: passwordはenv経由でpod specに平文露出させない
	// maxmemory等もenvで渡しdouble-quote参照, sh経由でも値のワード分割/コマンド注入を防ぐ
	// キュー耐久化のためAOFを既定有効
	redisCmd := `exec redis-server --requirepass "$REDIS_PASSWORD" --maxmemory "$MAXMEMORY" --maxmemory-policy "$MAXMEMORY_POLICY"`
	if inst.appendOnly {
		redisCmd += " --appendonly yes"
	}
	authSel := redisAuthSecretKeySelector(m)
	redisEnv := []corev1.EnvVar{
		// server用requirepassとredis-cli(probe)用のAUTHを同一secretから
		secretEnv("REDIS_PASSWORD", authSel),
		secretEnv("REDISCLI_AUTH", authSel),
		{Name: "MAXMEMORY", Value: inst.maxMemory},
		{Name: "MAXMEMORY_POLICY", Value: inst.maxMemoryPolicy},
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if err := r.applyStatefulSet(ctx, m, sts, func() error {
		sts.Labels = labelsFor(m, comp)
		sts.Spec.ServiceName = name
		sts.Spec.Replicas = int32Ptr(1)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, comp)}
		sts.Spec.Template.Labels = labelsFor(m, comp)
		// requirepassのローテーション(値変化=resourceVersion変化)でpodをrollし新passを取り込む
		ver, err := r.secretVersion(ctx, m.Namespace, nameRedisAuthSecret(m))
		if err != nil {
			return err
		}
		sts.Spec.Template.Annotations = checksumAnnotation(ver)
		containers := []corev1.Container{
			{
				Name:            "redis",
				Image:           inst.image,
				Command:         []string{"sh", "-c", redisCmd},
				Env:             redisEnv,
				SecurityContext: restrictedContainerSecurityContext(),
				Resources:       resourcesOr(inst.resources, "50m", "128Mi", "512Mi"),
				Ports:           []corev1.ContainerPort{{ContainerPort: redisPort}},
				ReadinessProbe:  redisPingProbe(10),
				LivenessProbe:   redisPingProbe(20),
				VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}, tmpMount()},
			},
		}
		// monitoring時はredis_exporter sidecar(requirepassをREDIS_PASSWORDで認証)
		if monitoringEnabled(m) {
			containers = append(containers, redisExporterContainer(m))
		}
		sts.Spec.Template.Spec = corev1.PodSpec{
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext:              nonRootPodSecurityContext(redisUID),
			Containers:                   containers,
			Volumes:                      []corev1.Volume{tmpVolume()},
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
	// standalone redisはinstance全体のnetwork.isolation(intra-instance)で保護する
	// 旧版が作っていた専用NP(<name>-redis)があれば掃除
	oldNP := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: nameRedisInstance(m, inst.suffix), Namespace: m.Namespace}}
	return r.deleteIfExists(ctx, oldNP)
}

// deleteRedisStandalone: 指定suffixのstandalone資源(STS/Service/NetworkPolicy)を掃除
func (r *MisskeyReconciler) deleteRedisStandalone(ctx context.Context, m *misskeyv1beta1.Misskey, suffix string) error {
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
