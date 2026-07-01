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
	"strings"

	corev1 "k8s.io/api/core/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

const (
	roleApp    = "app"
	roleWorker = "worker"

	misskeyUID = 1000
)

// secretEnv builds an EnvVar sourced from a secret key.
func secretEnv(name string, sel corev1.SecretKeySelector) corev1.EnvVar {
	s := sel
	return corev1.EnvVar{
		Name:      name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &s},
	}
}

// renderInitEnv returns the env vars the render-config init container needs to
// substitute the ${...} placeholders in default.yml.
func renderInitEnv(p plan) []corev1.EnvVar {
	env := []corev1.EnvVar{secretEnv("DB_PASSWORD", p.dbPassSel)}
	if p.meiliEnabled {
		env = append(env, secretEnv("MEILI_KEY", p.meiliKeySel))
	}
	if p.redisPassSel != nil {
		env = append(env, secretEnv("REDIS_PASSWORD", *p.redisPassSel))
	}
	if p.setupEnabled {
		env = append(env, secretEnv("SETUP_PASSWORD", p.setupSel))
	}
	return env
}

// renderSedCommand builds the sed pipeline that expands secret placeholders.
func renderSedCommand(p plan) string {
	exprs := []string{`-e "s|\${DB_PASSWORD}|${DB_PASSWORD}|g"`}
	if p.meiliEnabled {
		exprs = append(exprs, `-e "s|\${MEILI_KEY}|${MEILI_KEY}|g"`)
	}
	if p.redisPassSel != nil {
		exprs = append(exprs, `-e "s|\${REDIS_PASSWORD}|${REDIS_PASSWORD}|g"`)
	}
	if p.setupEnabled {
		exprs = append(exprs, `-e "s|\${SETUP_PASSWORD}|${SETUP_PASSWORD}|g"`)
	}
	return "set -eu\nsed " + strings.Join(exprs, " ") + " /tpl/default.yml > /shared/default.yml"
}

// buildMisskeyPodSpec builds the shared PodSpec for the app and worker roles.
func buildMisskeyPodSpec(m *misskeyv1alpha1.Misskey, p plan, role string, comp misskeyv1alpha1.ComponentSpec) corev1.PodSpec {
	env := []corev1.EnvVar{
		{Name: "COREPACK_INTEGRITY_KEYS", Value: "0"},
		{Name: "MK_DISABLE_CLUSTERING", Value: "true"},
	}
	var ports []corev1.ContainerPort
	if role == roleApp {
		env = append(env, corev1.EnvVar{Name: "MK_ONLY_SERVER", Value: "true"})
		ports = []corev1.ContainerPort{{ContainerPort: misskeyPort}}
	} else {
		env = append(env, corev1.EnvVar{Name: "MK_ONLY_QUEUE", Value: "true"})
	}

	spread := spreadConstraints(labelsFor(m, role))

	return corev1.PodSpec{
		ImagePullSecrets:          m.Spec.ImagePullSecrets,
		SecurityContext:           nonRootPodSecurityContext(misskeyUID),
		TopologySpreadConstraints: spread,
		NodeSelector:              comp.NodeSelector,
		Tolerations:               comp.Tolerations,
		InitContainers: []corev1.Container{
			{
				// Copy built/ into a writable emptyDir; Misskey may write there at boot.
				Name:            "prepare-built",
				Image:           m.Spec.Image,
				Command:         []string{"sh", "-c", "cp -r /misskey/built/. /tmp/built/"},
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "built-volume", MountPath: "/tmp/built"},
				},
			},
			{
				// Expand ${...} secret placeholders in default.yml at pod start.
				Name:            "render-config",
				Image:           "alpine:3",
				Command:         []string{"sh", "-c", renderSedCommand(p)},
				Env:             renderInitEnv(p),
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config-tpl", MountPath: "/tpl"},
					{Name: "config-rendered", MountPath: "/shared"},
				},
			},
		},
		Containers: []corev1.Container{
			{
				Name:            role,
				Image:           m.Spec.Image,
				SecurityContext: restrictedContainerSecurityContext(),
				Resources:       comp.Resources,
				Env:             env,
				Ports:           ports,
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "config-rendered",
						MountPath: "/misskey/.config/default.yml",
						SubPath:   "default.yml",
						ReadOnly:  true,
					},
					{Name: "built-volume", MountPath: "/misskey/built"},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "config-tpl",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: nameConfig(m)},
					},
				},
			},
			{Name: "config-rendered", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "built-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}
