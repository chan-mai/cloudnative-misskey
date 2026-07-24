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
	"fmt"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// KEDAのGVK。queue深度スケールにScaledObject(+認証時TriggerAuthentication)を使う
var (
	scaledObjectGVK = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
	triggerAuthGVK  = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "TriggerAuthentication"}
)

// scaleConfig: app/worker各autoscaling型の共通ビュー(queues/rpsを畳んで扱う)
type scaleConfig struct {
	misskeyv1beta1.AutoscalingSpec
	queues []misskeyv1beta1.QueueScaleTrigger
	rps    *misskeyv1beta1.RPSTrigger
}

// appScaleConfig: AppAutoscalingSpec→scaleConfig(nil透過)
func appScaleConfig(a *misskeyv1beta1.AppAutoscalingSpec) *scaleConfig {
	if a == nil {
		return nil
	}
	return &scaleConfig{AutoscalingSpec: a.AutoscalingSpec, rps: a.RPS}
}

// workerScaleConfig: WorkerAutoscalingSpec→scaleConfig(nil透過)
func workerScaleConfig(a *misskeyv1beta1.WorkerAutoscalingSpec) *scaleConfig {
	if a == nil {
		return nil
	}
	return &scaleConfig{AutoscalingSpec: a.AutoscalingSpec, queues: a.Queues}
}

// autoscalingEnabled: autoscalingブロックがあり有効か
func autoscalingEnabled(a *scaleConfig) bool {
	return a != nil
}

// autoscalingUsesKEDA: queue/rps trigger指定でKEDA、なければnative HPA
func autoscalingUsesKEDA(a *scaleConfig) bool {
	return len(a.queues) > 0 || a.rps != nil
}

// nameTriggerAuth: KEDA TriggerAuthentication名(redis認証用, managed/external問わず)
func nameTriggerAuth(targetName string) string { return targetName + "-redis-auth" }

// reconcileAutoscaler: componentのHPA/ScaledObjectを望ましい状態へ収束、無効/mode切替で他方を掃除
func (r *MisskeyReconciler) reconcileAutoscaler(ctx context.Context, m *misskeyv1beta1.Misskey, component, targetName string, a *scaleConfig, p plan) error {
	if !autoscalingEnabled(a) {
		if err := r.deleteHPA(ctx, m, targetName); err != nil {
			return err
		}
		return r.deleteScaledObject(ctx, m, targetName)
	}
	if autoscalingUsesKEDA(a) {
		// mode切替時のnative HPA掃除
		if err := r.deleteHPA(ctx, m, targetName); err != nil {
			return err
		}
		return r.reconcileScaledObject(ctx, m, component, targetName, a, p)
	}
	// native HPA(queueなし)。ScaledObject/TriggerAuthを掃除
	if err := r.deleteScaledObject(ctx, m, targetName); err != nil {
		return err
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: m.Namespace}}
	return r.apply(ctx, m, hpa, func() error {
		hpa.Labels = labelsFor(m, component)
		hpa.Spec = buildHPASpec(targetName, a)
		return nil
	})
}

// buildHPASpec: native HPA(autoscaling/v2)のspec。cpu/memory metric(未指定はcpu80%)
func buildHPASpec(targetName string, a *scaleConfig) autoscalingv2.HorizontalPodAutoscalerSpec {
	var metrics []autoscalingv2.MetricSpec
	if a.TargetCPUUtilizationPercentage != nil {
		metrics = append(metrics, resourceUtilMetric(corev1.ResourceCPU, *a.TargetCPUUtilizationPercentage))
	}
	if a.TargetMemoryUtilizationPercentage != nil {
		metrics = append(metrics, resourceUtilMetric(corev1.ResourceMemory, *a.TargetMemoryUtilizationPercentage))
	}
	if len(metrics) == 0 {
		metrics = append(metrics, resourceUtilMetric(corev1.ResourceCPU, 80))
	}
	return autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: targetName,
		},
		MinReplicas: a.MinReplicas, // nilはserver側で1にdefault
		MaxReplicas: a.MaxReplicas,
		Metrics:     metrics,
	}
}

// resourceUtilMetric: resource(cpu/memory)のUtilization metric
func resourceUtilMetric(name corev1.ResourceName, target int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32Ptr(target),
			},
		},
	}
}

// reconcileScaledObject: KEDA ScaledObject(+redis認証時TriggerAuthentication)をapply
// TriggerAuthはqueue triggerが参照する時のみ(rps単独では不要)
func (r *MisskeyReconciler) reconcileScaledObject(ctx context.Context, m *misskeyv1beta1.Misskey, component, targetName string, a *scaleConfig, p plan) error {
	ep := jobQueueEndpoint(p)
	if ep.passSel != nil && len(a.queues) > 0 {
		if err := r.applySSA(ctx, m, buildTriggerAuth(m, targetName, ep)); err != nil {
			return err
		}
	} else if err := r.deleteTriggerAuth(ctx, m, targetName); err != nil {
		return err
	}
	return r.applySSA(ctx, m, buildScaledObject(m, component, targetName, a, ep))
}

// jobQueueEndpoint: worker queueが載るredis。jobQueueロール分離時はそれ、無ければdefault redis
func jobQueueEndpoint(p plan) redisEndpoint {
	if ep, ok := p.redisRoles["jobQueue"]; ok {
		return ep
	}
	return p.redisDefault
}

// redisQueueListName: BullMQ待ちリストのRedisキー(computed default、listNameで上書き可)
// Misskey ormconfig: BullMQ prefix=`<redisPrefix>:queue:<queue>`、redisPrefix既定=URL host
// BullMQは `<prefix>:<name>:<type>` でキー生成のため queue名が2回現れる: <host>:queue:<queue>:<queue>:wait
func redisQueueListName(m *misskeyv1beta1.Misskey, queue string) string {
	return fmt.Sprintf("%s:queue:%s:%s:wait", hostFromURL(m.Spec.URL), queue, queue)
}

// defaultRPSQuery: 自インスタンスのproxy(Caddy)合計RPS
// ServiceMonitorはrelabelingなしのためnamespace/serviceラベルで自インスタンスに限定
func defaultRPSQuery(m *misskeyv1beta1.Misskey) string {
	return fmt.Sprintf(`sum(rate(caddy_http_request_duration_seconds_count{namespace=%q,service=%q}[2m]))`, m.Namespace, nameProxy(m))
}

// buildScaledObject: KEDA ScaledObject unstructured。各queueをredis/redis-sentinel triggerに
func buildScaledObject(m *misskeyv1beta1.Misskey, component, targetName string, a *scaleConfig, ep redisEndpoint) *unstructured.Unstructured {
	sentinel := len(ep.sentinels) > 0
	triggers := make([]any, 0, len(a.queues)+1)
	for _, q := range a.queues {
		listName := q.ListName
		if listName == "" {
			listName = redisQueueListName(m, q.Name)
		}
		meta := map[string]any{
			"listName":      listName,
			"listLength":    strconv.Itoa(int(q.ListLength)),
			"databaseIndex": strconv.Itoa(int(ep.db)),
			"enableTLS":     strconv.FormatBool(ep.enableTLS),
		}
		typ := "redis"
		if sentinel {
			typ = "redis-sentinel"
			hosts, ports := sentinelHostsPorts(ep, m.Namespace)
			meta["hosts"] = hosts
			meta["ports"] = ports
			meta["sentinelMaster"] = ep.masterName
		} else {
			meta["address"] = redisTriggerAddress(ep, m.Namespace)
		}
		trig := map[string]any{"type": typ, "metadata": meta}
		if ep.passSel != nil {
			trig["authenticationRef"] = map[string]any{"name": nameTriggerAuth(targetName)}
		}
		triggers = append(triggers, trig)
	}
	// RPS trigger(任意): 前段proxyのリクエストレートでスケール
	if rps := a.rps; rps != nil {
		triggers = append(triggers, map[string]any{
			"type": "prometheus",
			"metadata": map[string]any{
				"serverAddress": rps.ServerAddress,
				"query":         stringOr(rps.Query, defaultRPSQuery(m)),
				"threshold":     strconv.Itoa(int(rps.TargetRPS)),
			},
		})
	}
	// cpu floor(任意)
	if a.TargetCPUUtilizationPercentage != nil {
		triggers = append(triggers, map[string]any{
			"type":     "cpu",
			"metadata": map[string]any{"type": "Utilization", "value": strconv.Itoa(int(*a.TargetCPUUtilizationPercentage))},
		})
	}

	// MinReplicas未指定はKEDA既定0だが、godoc/app HPAと揃え既定1にする(0スケールはidle後の初回遅延あり)
	minReplicas := int64(1)
	if a.MinReplicas != nil {
		minReplicas = int64(*a.MinReplicas)
	}
	spec := map[string]any{
		"scaleTargetRef":  map[string]any{"name": targetName},
		"minReplicaCount": minReplicas,
		"maxReplicaCount": int64(a.MaxReplicas),
		"triggers":        triggers,
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(scaledObjectGVK)
	u.SetName(targetName)
	u.SetNamespace(m.Namespace)
	u.SetLabels(labelsFor(m, component))
	u.Object["spec"] = spec
	return u
}

// redisTriggerAddress: KEDA redis triggerのaddress
// managedはService名のためkeda namespaceからのcross-ns解決にFQDN化。externalはhostをそのまま使う
func redisTriggerAddress(ep redisEndpoint, namespace string) string {
	if ep.managed {
		return fmt.Sprintf("%s.%s.svc:%d", ep.host, namespace, ep.port)
	}
	return fmt.Sprintf("%s:%d", ep.host, ep.port)
}

// sentinelHostsPorts: hosts/portsの並行CSV(KEDA redis-sentinel用)
// managedのsentinelはService名のためFQDN化。externalはhostをそのまま使う
func sentinelHostsPorts(ep redisEndpoint, namespace string) (string, string) {
	hosts := make([]string, 0, len(ep.sentinels))
	ports := make([]string, 0, len(ep.sentinels))
	for _, s := range ep.sentinels {
		host := s.host
		if ep.managed {
			host = fmt.Sprintf("%s.%s.svc", s.host, namespace)
		}
		hosts = append(hosts, host)
		ports = append(ports, strconv.Itoa(int(s.port)))
	}
	return strings.Join(hosts, ","), strings.Join(ports, ",")
}

// buildTriggerAuth: redisパスワードをKEDAへ渡すTriggerAuthentication(managed/external問わず)
func buildTriggerAuth(m *misskeyv1beta1.Misskey, targetName string, ep redisEndpoint) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(triggerAuthGVK)
	u.SetName(nameTriggerAuth(targetName))
	u.SetNamespace(m.Namespace)
	u.SetLabels(labelsFor(m, "autoscaler"))
	refs := []any{
		map[string]any{"parameter": "password", "name": ep.passSel.Name, "key": ep.passSel.Key},
	}
	// sentinel(HA)経路はsentinel portもrequirepass。KEDAがsentinelへauthするため必要
	if len(ep.sentinels) > 0 {
		refs = append(refs, map[string]any{"parameter": "sentinelPassword", "name": ep.passSel.Name, "key": ep.passSel.Key})
	}
	u.Object["spec"] = map[string]any{"secretTargetRef": refs}
	return u
}

// deleteHPA / deleteScaledObject / deleteTriggerAuth: 無効化・mode切替時の掃除
func (r *MisskeyReconciler) deleteHPA(ctx context.Context, m *misskeyv1beta1.Misskey, targetName string) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: m.Namespace}}
	return r.deleteIfExists(ctx, hpa)
}

func (r *MisskeyReconciler) deleteScaledObject(ctx context.Context, m *misskeyv1beta1.Misskey, targetName string) error {
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(scaledObjectGVK)
	so.SetName(targetName)
	so.SetNamespace(m.Namespace)
	if err := r.deleteIfExists(ctx, so); err != nil {
		return err
	}
	return r.deleteTriggerAuth(ctx, m, targetName)
}

func (r *MisskeyReconciler) deleteTriggerAuth(ctx context.Context, m *misskeyv1beta1.Misskey, targetName string) error {
	ta := &unstructured.Unstructured{}
	ta.SetGroupVersionKind(triggerAuthGVK)
	ta.SetName(nameTriggerAuth(targetName))
	ta.SetNamespace(m.Namespace)
	return r.deleteIfExists(ctx, ta)
}
