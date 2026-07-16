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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

const (
	instanceLabel  = "app.kubernetes.io/instance"
	componentLabel = "app.kubernetes.io/component"
	nsNameLabel    = "kubernetes.io/metadata.name"
)

// reconcileTenancy: instance隔離(ingress/egress)と、専用namespace前提のResourceQuota/LimitRange
func (r *MisskeyReconciler) reconcileTenancy(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	if err := r.reconcileNetworkIsolation(ctx, m); err != nil {
		return err
	}
	if err := r.reconcileEgressIsolation(ctx, m); err != nil {
		return err
	}
	if err := r.reconcileResourceQuota(ctx, m); err != nil {
		return err
	}
	return r.reconcileLimitRange(ctx, m)
}

// reconcileNetworkIsolation: backend podへのingressをintra-instanceに限る
// 公開入口(proxy有効時proxy、無効時app)は開放し、ingress controllerから到達可能に保つ
// postgresはCNPG operatorのcross-namespaceアクセス(instance manager :8000)が要るため除外
func (r *MisskeyReconciler) reconcileNetworkIsolation(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-isolation", Namespace: m.Namespace}}
	if !boolOr(m.Spec.Network.Isolation.Enabled, true) {
		return r.deleteIfExists(ctx, np)
	}
	publicEntry := "proxy"
	if !boolOr(m.Spec.Proxy.Enabled, true) {
		publicEntry = roleApp
	}
	// intra-instance + 明示allowNamespace(監視等)からのingressを許可
	from := []networkingv1.NetworkPolicyPeer{
		{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: m.Name}}},
	}
	for _, ns := range m.Spec.Network.Isolation.AllowedNamespaces {
		from = append(from, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nsNameLabel: ns}},
		})
	}
	return r.apply(ctx, m, np, func() error {
		np.Labels = labelsFor(m, "isolation")
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{instanceLabel: m.Name},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      componentLabel,
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{publicEntry, "postgres"},
			}},
		}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: from}}
		return nil
	})
}

// reconcileEgressIsolation: opt-in。app/worker=DNS+intra+public(private/metadata除く)、
// その他backend=DNS+intraのみ。postgresは除外(CNPGのbackup/replicationのため)
func (r *MisskeyReconciler) reconcileEgressIsolation(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	frontend := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-egress-frontend", Namespace: m.Namespace}}
	backend := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-egress-backend", Namespace: m.Namespace}}
	if !boolOr(m.Spec.Network.EgressIsolation.Enabled, false) {
		if err := r.deleteIfExists(ctx, frontend); err != nil {
			return err
		}
		return r.deleteIfExists(ctx, backend)
	}
	common := egressCommonRules(m)

	// frontend: app/worker。連合のためpublic egressを許す
	if err := r.apply(ctx, m, frontend, func() error {
		frontend.Labels = labelsFor(m, "egress")
		frontend.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{instanceLabel: m.Name},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      componentLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{roleApp, roleWorker},
			}},
		}
		frontend.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		// CNPGはpooler podのapp.kubernetes.io/instanceをクラスタ名で上書きするためintra-instance則に載らない
		// cnpg.io/clusterでdb/pooler pod宛egressを別途許可(app/worker→pooler-rw/ro疎通)
		egress := append(common, dbEgressRule(m))
		// redis HA operator管理pod(app=<CR名>、instance label無し)宛も同様に許可
		if rr := redisEgressRule(m); rr != nil {
			egress = append(egress, *rr)
		}
		frontend.Spec.Egress = append(egress, publicEgressRule())
		return nil
	}); err != nil {
		return err
	}

	// backend: app/worker/postgres以外。外向き不要
	return r.apply(ctx, m, backend, func() error {
		backend.Labels = labelsFor(m, "egress")
		backend.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{instanceLabel: m.Name},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      componentLabel,
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{roleApp, roleWorker, "postgres"},
			}},
		}
		backend.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		backend.Spec.Egress = common
		return nil
	})
}

// egressCommonRules: DNS(:53) + intra-instance + 明示allowNamespace。全componentで共通
func egressCommonRules(m *misskeyv1beta1.Misskey) []networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dnsPort := intstr.FromInt32(53)
	dnsNs := stringOr(m.Spec.Network.EgressIsolation.DNSNamespace, "kube-system")
	rules := []networkingv1.NetworkPolicyEgressRule{
		{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nsNameLabel: dnsNs}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
		},
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: m.Name}}}}},
	}
	for _, ns := range m.Spec.Network.EgressIsolation.AllowedNamespaces {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{nsNameLabel: ns}}}},
		})
	}
	return rules
}

// dbEgressRule: CNPGクラスタ配下のpod(db instance + pooler)宛egressを許可
// pooler podはCNPGがapp.kubernetes.io/instanceを上書きするためinstanceLabelでは拾えず、
// CNPG固有のcnpg.io/clusterで選択する。managed DB専用(externalはnameDBのクラスタ不在でno-op)
func dbEgressRule(m *misskeyv1beta1.Misskey) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": nameDB(m)}},
		}},
	}
}

// redisHAPodApps: HA operator管理redis/sentinel podのapp label値一覧
// RedisReplication pod=app:<CR名>、RedisSentinel pod=app:<CR名>-sentinel
func redisHAPodApps(m *misskeyv1beta1.Misskey) []string {
	var apps []string
	for _, inst := range managedRedisInstances(m) {
		if inst.ha {
			apps = append(apps, nameRedisHA(m, inst.suffix), nameRedisSentinelService(m, inst.suffix))
		}
	}
	return apps
}

// redisEgressRule: HA operator管理pod宛egressを許可(app.kubernetes.io/instanceを持たないため)
// HAインスタンスが無ければnil
func redisEgressRule(m *misskeyv1beta1.Misskey) *networkingv1.NetworkPolicyEgressRule {
	apps := redisHAPodApps(m)
	if len(apps) == 0 {
		return nil
	}
	return &networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app",
					Operator: metav1.LabelSelectorOpIn,
					Values:   apps,
				}},
			},
		}},
	}
}

// publicEgressRule: private/CGNAT/link-local(metadata)を除くpublicへのegress
// IPv4のみ対応。dual-stackクラスタのIPv6 egressは本ruleに乗らず遮断される(fail-closed、README制限事項参照)
func publicEgressRule() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
			CIDR:   "0.0.0.0/0",
			Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"},
		}}},
	}
}

// reconcileResourceQuota: dedicated時のみnamespace-wideなResourceQuotaを生成
func (r *MisskeyReconciler) reconcileResourceQuota(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	rq := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-quota", Namespace: m.Namespace}}
	if !m.Spec.Tenancy.Dedicated || len(m.Spec.Tenancy.Quota) == 0 {
		return r.deleteIfExists(ctx, rq)
	}
	return r.apply(ctx, m, rq, func() error {
		rq.Labels = labelsFor(m, "tenancy")
		rq.Spec.Hard = m.Spec.Tenancy.Quota
		return nil
	})
}

// reconcileLimitRange: dedicated時のみContainer既定/上限のLimitRangeを生成
func (r *MisskeyReconciler) reconcileLimitRange(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-limits", Namespace: m.Namespace}}
	spec := m.Spec.Tenancy.LimitRange
	if !m.Spec.Tenancy.Dedicated || spec == nil {
		return r.deleteIfExists(ctx, lr)
	}
	return r.apply(ctx, m, lr, func() error {
		lr.Labels = labelsFor(m, "tenancy")
		lr.Spec.Limits = []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Default:        spec.Default,
			DefaultRequest: spec.DefaultRequest,
			Max:            spec.Max,
		}}
		return nil
	})
}

// deleteIfExists: 存在すれば削除(オプション無効化時のcleanup用)
// CRD未導入(オプションoperator=CNPG/redis-operator/KEDA不在)時のNoMatchも無視し、
// HA/pooler/autoscaling未使用のインスタンスがそれら未インストール環境でも壊れないようにする
func (r *MisskeyReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return err
	}
	return nil
}
