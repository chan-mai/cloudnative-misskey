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
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// buildCaddyPodSpec builds the pod spec shared by the proxy and maintenance
// Deployments. Caddy runs non-root, so the binary is copied into a writable
// emptyDir and /data + /config are emptyDirs.
func buildCaddyPodSpec(m *misskeyv1alpha1.Misskey, caddyfileKey string, withMaintenanceHTML bool) corev1.PodSpec {
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
		SecurityContext: nonRootPodSecurityContext(genericNonRootUID),
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

// reconcileProxy creates/updates the proxy Service+Deployment and, when enabled,
// the maintenance Service+Deployment.
func (r *MisskeyReconciler) reconcileProxy(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	// proxy Service (port 80 -> 8080)
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
		pod := buildCaddyPodSpec(m, "Caddyfile", false)
		setDeployment(pdep, m, "proxy", replicasOr(m.Spec.Proxy.Replicas, 2), pod, checksumAnnotation(renderCaddyfile(m)))
		return nil
	}); err != nil {
		return err
	}
	if err := r.reconcilePDB(ctx, m, "proxy"); err != nil {
		return err
	}

	if !boolOr(m.Spec.Proxy.Maintenance.Enabled, true) {
		return nil
	}

	// maintenance Service (port 8080 -> 8080)
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
		pod := buildCaddyPodSpec(m, "maintenance.Caddyfile", true)
		setDeployment(mdep, m, "maintenance", int32Ptr(1), pod, checksumAnnotation(renderMaintenanceCaddyfile(), maintenanceHTMLContent(m)))
		return nil
	})
}
