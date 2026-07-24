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
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// proxy Deploymentのpod specを生成
// Caddyは非rootで動くため、バイナリを書込可能なemptyDirにコピーし/dataと/configもemptyDirにする
func buildCaddyPodSpec(m *misskeyv1beta1.Misskey, withMaintenanceHTML bool) corev1.PodSpec {
	image := stringOr(m.Spec.Proxy.Image, "caddy:2")

	volumes := []corev1.Volume{
		{
			Name: "caddy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: nameConfig(m)},
				},
			},
		},
		{Name: "caddy-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "caddy-config-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "caddy-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		tmpVolume(),
	}

	mounts := []corev1.VolumeMount{
		{Name: "caddy-config", MountPath: "/etc/caddy/Caddyfile", SubPath: "Caddyfile", ReadOnly: true},
		{Name: "caddy-data", MountPath: "/data"},
		{Name: "caddy-config-tmp", MountPath: "/config"},
		{Name: "caddy-bin", MountPath: "/caddy-bin"},
		tmpMount(),
	}

	if withMaintenanceHTML {
		volumes = append(volumes, corev1.Volume{
			Name: "maintenance-html",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: nameMaintenanceHTML(m)},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "maintenance-html", MountPath: "/usr/share/caddy", ReadOnly: true})
	}

	ports := []corev1.ContainerPort{
		{ContainerPort: proxyPort},
		{Name: "metrics", ContainerPort: proxyMetricsPort},
	}

	return corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		SecurityContext:              nonRootPodSecurityContext(genericNonRootUID),
		TopologySpreadConstraints:    spreadConstraints(labelsFor(m, "proxy")),
		InitContainers: []corev1.Container{
			{
				Name:            "prepare-caddy",
				Image:           image,
				Command:         []string{"sh", "-c", "cp /usr/bin/caddy /caddy-bin/caddy"},
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "caddy-bin", MountPath: "/caddy-bin"}},
			},
		},
		Containers: []corev1.Container{
			{
				Name:            "caddy",
				Image:           image,
				Command:         []string{"/caddy-bin/caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
				SecurityContext: restrictedContainerSecurityContext(),
				Resources:       resourcesOr(m.Spec.Proxy.Resources, "10m", "32Mi", "128Mi"),
				Ports:           ports,
				VolumeMounts:    mounts,
			},
		},
		Volumes: volumes,
	}
}

// proxyのService+Deploymentを作成/更新。メンテページはproxy自身のfile_serverが配信
// proxy/maintenance無効化時は該当リソースを掃除(reconcileRedis等のopt-outパターンと同じ)
func (r *MisskeyReconciler) reconcileProxy(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	// 統合前の<name>-maintenance Deployment/Serviceをアップグレード互換のため常に掃除
	if err := r.deleteLegacyMaintenanceWorkload(ctx, m); err != nil {
		return err
	}

	if !boolOr(m.Spec.Proxy.Enabled, true) {
		if err := r.deleteProxyResources(ctx, m); err != nil {
			return err
		}
		return r.deleteMaintenanceHTMLConfigMap(ctx, m)
	}

	// proxy Service(port 80 -> 8080)
	psvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, psvc, func() error {
		psvc.Labels = labelsFor(m, "proxy")
		psvc.Spec.Selector = selectorFor(m, "proxy")
		psvc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromInt32(proxyPort),
		}}
		// monitoring時はCaddyのmetricsポートを公開(ServiceMonitorがscrape)
		if monitoringEnabled(m) {
			psvc.Spec.Ports = append(psvc.Spec.Ports, corev1.ServicePort{
				Name:       "metrics",
				Port:       proxyMetricsPort,
				TargetPort: intstr.FromInt32(proxyMetricsPort),
			})
		}
		return nil
	}); err != nil {
		return err
	}

	maint := boolOr(m.Spec.Proxy.Maintenance.Enabled, true)
	pdep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, pdep, func() error {
		pod := buildCaddyPodSpec(m, maint)
		// maintenance有効時のみHTMLをchecksumに含め、無効時のhtml編集で無駄にロールさせない
		parts := []string{renderCaddyfile(m)}
		if maint {
			parts = append(parts, maintenanceHTMLContent(m))
		}
		setDeployment(pdep, m, "proxy", replicasOr(m.Spec.Proxy.Replicas, 2), pod, checksumAnnotation(parts...))
		return nil
	}); err != nil {
		return err
	}
	if err := r.reconcilePDB(ctx, m, "proxy"); err != nil {
		return err
	}

	if !maint {
		return r.deleteMaintenanceHTMLConfigMap(ctx, m)
	}
	return nil
}

// deleteProxyResources: proxy無効化時のcleanup(Deployment/Service/PDB)
// config CMのCaddyfileキーはreconcileConfigMapsが落とすため、Deploymentを残すとmount切れになる
func (r *MisskeyReconciler) deleteProxyResources(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}},
	}
	for _, o := range objs {
		if err := r.deleteIfExists(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// deleteLegacyMaintenanceWorkload: 統合前構成のmaintenance Deployment/Serviceを掃除
// CRのownerRefがありCR存続中はGCされないため明示削除(数リリース後に削除可)
func (r *MisskeyReconciler) deleteLegacyMaintenanceWorkload(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}},
	}
	for _, o := range objs {
		if err := r.deleteIfExists(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// deleteMaintenanceHTMLConfigMap: maintenance/proxy無効化時のHTML ConfigMap掃除
func (r *MisskeyReconciler) deleteMaintenanceHTMLConfigMap(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenanceHTML(m), Namespace: m.Namespace}}
	return r.deleteIfExists(ctx, cm)
}
