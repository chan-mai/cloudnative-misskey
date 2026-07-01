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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// Well-known port numbers used across the instance.
const (
	misskeyPort      = 3000
	proxyPort        = 8080
	redisPort        = 6379
	meiliPort        = 7700
	postgresPort     = 5432
	meiliMasterKeyID = "MEILI_MASTER_KEY"
	setupPasswordID  = "SETUP_PASSWORD"
)

// labelsFor returns the standard label set for a component of an instance.
func labelsFor(m *misskeyv1alpha1.Misskey, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "misskey",
		"app.kubernetes.io/instance":   m.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "cloud-native-misskey",
	}
}

// selectorFor returns a minimal, immutable label selector for a component.
// Kept smaller than labelsFor so labels can evolve without breaking selectors.
func selectorFor(m *misskeyv1alpha1.Misskey, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance":  m.Name,
		"app.kubernetes.io/component": component,
	}
}

// Child object names, all derived deterministically from the instance name.
func nameApp(m *misskeyv1alpha1.Misskey) string             { return m.Name + "-app" }
func nameWorker(m *misskeyv1alpha1.Misskey) string          { return m.Name + "-worker" }
func nameProxy(m *misskeyv1alpha1.Misskey) string           { return m.Name + "-proxy" }
func nameMaintenance(m *misskeyv1alpha1.Misskey) string     { return m.Name + "-maintenance" }
func nameRedis(m *misskeyv1alpha1.Misskey) string           { return m.Name + "-redis" }
func nameMeili(m *misskeyv1alpha1.Misskey) string           { return m.Name + "-meilisearch" }
func nameDB(m *misskeyv1alpha1.Misskey) string              { return m.Name + "-db" }
func nameConfig(m *misskeyv1alpha1.Misskey) string          { return m.Name + "-config" }
func nameMaintenanceHTML(m *misskeyv1alpha1.Misskey) string { return m.Name + "-maintenance-html" }
func nameSetup(m *misskeyv1alpha1.Misskey) string           { return m.Name + "-setup" }

// nameDBService is the CNPG-generated read-write service for the cluster.
func nameDBService(m *misskeyv1alpha1.Misskey) string { return nameDB(m) + "-rw" }

// nameDBAppSecret is the CNPG-generated app credentials secret for the cluster.
func nameDBAppSecret(m *misskeyv1alpha1.Misskey) string { return nameDB(m) + "-app" }

// int32Ptr returns a pointer to v.
func int32Ptr(v int32) *int32 { return &v }

// int64Ptr returns a pointer to v.
func int64Ptr(v int64) *int64 { return &v }

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool { return &v }

// replicasOr returns *p or def when p is nil.
func replicasOr(p *int32, def int32) *int32 {
	if p == nil {
		return int32Ptr(def)
	}
	return p
}

// boolOr returns *p or def when p is nil.
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// stringOr returns s or def when s is empty.
func stringOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// quantityOr returns q or the parsed def when q is zero.
func quantityOr(q resource.Quantity, def string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(def)
	}
	return q
}

// nonRootPodSecurityContext is the hardened pod-level security context reused by
// every workload the operator manages.
func nonRootPodSecurityContext(uid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(uid),
		FSGroup:        int64Ptr(uid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedContainerSecurityContext is the hardened container-level context.
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// spreadConstraints keeps replicas best-effort spread across nodes.
func spreadConstraints(matchLabels map[string]string) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}
