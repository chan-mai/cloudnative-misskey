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

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// proxyとmaintenance Deployment共通のpod specを生成
// Caddyは非rootで動くため、バイナリを書込可能なemptyDirにコピーし/dataと/configもemptyDirにする
func buildCaddyPodSpec(m *misskeyv1alpha1.Misskey, caddyfileKey string, withMaintenanceHTML bool, component string) corev1.PodSpec {
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
	}

	mounts := []corev1.VolumeMount{
		{Name: "caddy-config", MountPath: "/etc/caddy/Caddyfile", SubPath: caddyfileKey, ReadOnly: true},
		{Name: "caddy-data", MountPath: "/data"},
		{Name: "caddy-config-tmp", MountPath: "/config"},
		{Name: "caddy-bin", MountPath: "/caddy-bin"},
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

	return corev1.PodSpec{
		SecurityContext:           nonRootPodSecurityContext(genericNonRootUID),
		TopologySpreadConstraints: spreadConstraints(labelsFor(m, component)),
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
				Ports:           []corev1.ContainerPort{{ContainerPort: proxyPort}},
				VolumeMounts:    mounts,
			},
		},
		Volumes: volumes,
	}
}

// proxyのService+Deploymentを作成/更新し、有効時はmaintenanceのService+Deploymentも扱う
// proxy/maintenance無効化時は該当リソースを掃除(reconcileRedis等のopt-outパターンと同じ)
func (r *MisskeyReconciler) reconcileProxy(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	if !boolOr(m.Spec.Proxy.Enabled, true) {
		if err := r.deleteProxyResources(ctx, m); err != nil {
			return err
		}
		return r.deleteMaintenanceResources(ctx, m)
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
		return nil
	}); err != nil {
		return err
	}

	pdep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameProxy(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, pdep, func() error {
		pod := buildCaddyPodSpec(m, "Caddyfile", false, "proxy")
		setDeployment(pdep, m, "proxy", replicasOr(m.Spec.Proxy.Replicas, 2), pod, checksumAnnotation(renderCaddyfile(m)))
		return nil
	}); err != nil {
		return err
	}
	if err := r.reconcilePDB(ctx, m, "proxy"); err != nil {
		return err
	}

	if !boolOr(m.Spec.Proxy.Maintenance.Enabled, true) {
		return r.deleteMaintenanceResources(ctx, m)
	}

	// maintenance Service(port 8080 -> 8080)
	msvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, msvc, func() error {
		msvc.Labels = labelsFor(m, "maintenance")
		msvc.Spec.Selector = selectorFor(m, "maintenance")
		msvc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       proxyPort,
			TargetPort: intstr.FromInt32(proxyPort),
		}}
		return nil
	}); err != nil {
		return err
	}

	mdep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, mdep, func() error {
		pod := buildCaddyPodSpec(m, "maintenance.Caddyfile", true, "maintenance")
		setDeployment(mdep, m, "maintenance", int32Ptr(1), pod, checksumAnnotation(renderMaintenanceCaddyfile(), maintenanceHTMLContent(m)))
		return nil
	})
}

// deleteProxyResources: proxy無効化時のcleanup(Deployment/Service/PDB)
// config CMのCaddyfileキーはreconcileConfigMapsが落とすため、Deploymentを残すとmount切れになる
func (r *MisskeyReconciler) deleteProxyResources(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
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

// deleteMaintenanceResources: maintenance無効化時のcleanup(Deployment/Service/HTML ConfigMap)
func (r *MisskeyReconciler) deleteMaintenanceResources(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenance(m), Namespace: m.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenanceHTML(m), Namespace: m.Namespace}},
	}
	for _, o := range objs {
		if err := r.deleteIfExists(ctx, o); err != nil {
			return err
		}
	}
	return nil
}
