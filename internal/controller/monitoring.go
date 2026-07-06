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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
)

// Prometheus Operator CRD。SM=Service経由、PM=Pod直
var (
	serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	podMonitorGVK     = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}
)

const redisExporterPort = 9121

func monitoringEnabled(m *misskeyv1alpha1.Misskey) bool {
	return boolOr(m.Spec.Monitoring.Enabled, false)
}
func monitoringInterval(m *misskeyv1alpha1.Misskey) string {
	return stringOr(m.Spec.Monitoring.Interval, "30s")
}

// monitorLabels: operator標準label + ユーザ指定(Prometheus selector合わせ用)
func monitorLabels(m *misskeyv1alpha1.Misskey, component string) map[string]string {
	l := labelsFor(m, component)
	for k, v := range m.Spec.Monitoring.Labels {
		l[k] = v
	}
	return l
}

type metricsAuth struct{ name, key string }

// reconcileMonitoring: managed backendのServiceMonitor/PodMonitorをapply、無効/対象外はcleanup
func (r *MisskeyReconciler) reconcileMonitoring(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	on := monitoringEnabled(m)

	// PostgreSQL: CNPG podが:9187(port名metrics)でexpose
	pgName := nameDB(m) + "-metrics"
	if on && p.dbManaged {
		pm := buildPodMonitor(m, pgName, "postgres", map[string]string{"cnpg.io/cluster": nameDB(m)}, "metrics", nil)
		if err := r.applySSA(ctx, m, pm); err != nil {
			return err
		}
	} else if err := r.deleteIfExists(ctx, monitorObjRef(m, podMonitorGVK, pgName)); err != nil {
		return err
	}

	// MeiliSearch: /metrics(http port)、master keyでBearer認証
	meiliName := nameMeili(m) + "-metrics"
	if on && p.meiliManaged {
		sm := buildServiceMonitor(m, meiliName, "meilisearch", selectorFor(m, "meilisearch"), "http", "/metrics",
			&metricsAuth{name: p.meiliKeySel.Name, key: p.meiliKeySel.Key})
		if err := r.applySSA(ctx, m, sm); err != nil {
			return err
		}
	} else if err := r.deleteIfExists(ctx, monitorObjRef(m, serviceMonitorGVK, meiliName)); err != nil {
		return err
	}

	// Redis: standalone=redis_exporter sidecar(Service metrics port)をSM、HA=OT redisExporterをPM
	desired := map[string]bool{}
	if on {
		for _, inst := range managedRedisInstances(m) {
			name := nameRedisInstance(m, inst.suffix) + "-metrics"
			desired[name] = true
			comp := redisComponent(inst.suffix)
			if inst.ha {
				pm := buildPodMonitorPort(m, name, comp, map[string]string{"app": nameRedisHA(m, inst.suffix)}, redisExporterPort)
				if err := r.applySSA(ctx, m, pm); err != nil {
					return err
				}
			} else {
				sm := buildServiceMonitor(m, name, comp, selectorFor(m, comp), "metrics", "/metrics", nil)
				if err := r.applySSA(ctx, m, sm); err != nil {
					return err
				}
			}
		}
	}
	// 望ましくないredis monitorをcleanup(全suffix, SM/PM両方)
	for _, suffix := range allRedisSuffixes() {
		name := nameRedisInstance(m, suffix) + "-metrics"
		if desired[name] {
			continue
		}
		if err := r.deleteIfExists(ctx, monitorObjRef(m, serviceMonitorGVK, name)); err != nil {
			return err
		}
		if err := r.deleteIfExists(ctx, monitorObjRef(m, podMonitorGVK, name)); err != nil {
			return err
		}
	}
	return nil
}

// buildServiceMonitor: 指定Serviceのport/pathをscrape(auth任意)
func buildServiceMonitor(m *misskeyv1alpha1.Misskey, name, component string, sel map[string]string, port, path string, auth *metricsAuth) *unstructured.Unstructured {
	ep := map[string]any{"port": port, "path": path, "interval": monitoringInterval(m)}
	if auth != nil {
		ep["authorization"] = map[string]any{"credentials": map[string]any{"name": auth.name, "key": auth.key}}
	}
	spec := map[string]any{
		"selector":          map[string]any{"matchLabels": toAnyMap(sel)},
		"namespaceSelector": map[string]any{"matchNames": []any{m.Namespace}},
		"endpoints":         []any{ep},
	}
	return monitorObj(m, serviceMonitorGVK, name, component, spec)
}

// buildPodMonitor: 指定pod selectorのport名をscrape
func buildPodMonitor(m *misskeyv1alpha1.Misskey, name, component string, sel map[string]string, port string, auth *metricsAuth) *unstructured.Unstructured {
	ep := map[string]any{"port": port, "interval": monitoringInterval(m)}
	if auth != nil {
		ep["authorization"] = map[string]any{"credentials": map[string]any{"name": auth.name, "key": auth.key}}
	}
	spec := map[string]any{
		"selector":            map[string]any{"matchLabels": toAnyMap(sel)},
		"namespaceSelector":   map[string]any{"matchNames": []any{m.Namespace}},
		"podMetricsEndpoints": []any{ep},
	}
	return monitorObj(m, podMonitorGVK, name, component, spec)
}

// buildPodMonitorPort: port名でなくtargetPort(番号)でscrape(OT exporter等のport名不定用)
func buildPodMonitorPort(m *misskeyv1alpha1.Misskey, name, component string, sel map[string]string, targetPort int64) *unstructured.Unstructured {
	spec := map[string]any{
		"selector":          map[string]any{"matchLabels": toAnyMap(sel)},
		"namespaceSelector": map[string]any{"matchNames": []any{m.Namespace}},
		"podMetricsEndpoints": []any{map[string]any{
			"targetPort": targetPort,
			"interval":   monitoringInterval(m),
		}},
	}
	return monitorObj(m, podMonitorGVK, name, component, spec)
}

// monitorObj: GVK/name/ns/labels/specを備えたunstructured
func monitorObj(m *misskeyv1alpha1.Misskey, gvk schema.GroupVersionKind, name, component string, spec map[string]any) *unstructured.Unstructured {
	u := monitorObjRef(m, gvk, name)
	u.SetLabels(monitorLabels(m, component))
	u.Object["spec"] = spec
	return u
}

// monitorObjRef: GVK/name/nsのみのunstructured(cleanup用)
func monitorObjRef(m *misskeyv1alpha1.Misskey, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(m.Namespace)
	return u
}

// redisExporterContainer: standalone Redis用のredis_exporter sidecar(認証なし)
func redisExporterContainer(m *misskeyv1alpha1.Misskey) corev1.Container {
	return corev1.Container{
		Name:            "metrics",
		Image:           stringOr(m.Spec.Monitoring.RedisExporterImage, "oliver006/redis_exporter:v1.62.0-alpine"),
		Env:             []corev1.EnvVar{{Name: "REDIS_ADDR", Value: fmt.Sprintf("redis://localhost:%d", redisPort)}},
		Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: redisExporterPort}},
		SecurityContext: restrictedContainerSecurityContext(),
		Resources:       resourcesOr(corev1.ResourceRequirements{}, "10m", "16Mi", "64Mi"),
	}
}

// toAnyMap: map[string]string を unstructured用 map[string]any へ
func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
