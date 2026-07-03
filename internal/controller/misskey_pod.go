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
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

const (
	roleApp    = "app"
	roleWorker = "worker"

	// 公式Misskeyイメージが動作するuid(USER misskey)
	// 合わせることで/misskeyファイルとbuiltのemptyDirを書込可能に保つ
	misskeyUID = 991
)

// secretキー由来のEnvVarを生成
func secretEnv(name string, sel corev1.SecretKeySelector) corev1.EnvVar {
	s := sel
	return corev1.EnvVar{
		Name:      name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &s},
	}
}

// render-config initコンテナがdefault.ymlの${...}プレースホルダ置換に必要なenv varを返す
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

// default.ymlの${...}シークレットプレースホルダをリテラル文字列置換で展開するNode.jsプログラム
// String.split(literal).join(value)により、|, &, \, $, 改行を含む値で以前のsedパイプラインが壊れる(またはインジェクションを許す)原因の正規表現・シェル解釈を回避
// MisskeyイメージにNode同梱のため追加のツールイメージは不要
const renderConfigScript = `const fs = require('fs');
let s = fs.readFileSync('/tpl/default.yml', 'utf8');
for (const k of ['DB_PASSWORD', 'MEILI_KEY', 'REDIS_PASSWORD', 'SETUP_PASSWORD']) {
  const v = process.env[k];
  if (v !== undefined) s = s.split('${' + k + '}').join(v);
}
fs.writeFileSync('/shared/default.yml', s);`

// MisskeyサーバポートへのHTTP GET probeを生成
func httpProbe(path string, period, timeout, failure int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(misskeyPort)},
		},
		PeriodSeconds:    period,
		TimeoutSeconds:   timeout,
		FailureThreshold: failure,
	}
}

// app/workerロール共通のPodSpecを生成
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

	res := resourcesOr(comp.Resources, "100m", "400Mi", "800Mi")
	if role == roleWorker {
		res = resourcesOr(comp.Resources, "100m", "500Mi", "1Gi")
	}

	mainContainer := corev1.Container{
		Name:  role,
		Image: m.Spec.Image,
		// migrationはmigration Jobに一本化。既定CMDのmigrateandstartを使わずstartのみ
		Command:         []string{"pnpm", "run", "start"},
		SecurityContext: restrictedContainerSecurityContext(),
		Resources:       res,
		Env:             env,
		Ports:           ports,
		VolumeMounts:    misskeyConfigMounts(),
	}
	// appはHTTPを提供するため、readinessと再起動をMisskeyのヘルスエンドポイントで制御
	// startupProbeが遅い初回起動(DBマイグレーション)を吸収
	// workerはキュー専用でリスナもServiceもないためprobeなし
	if role == roleApp {
		const healthPath = "/api/server-info"
		mainContainer.StartupProbe = httpProbe(healthPath, 10, 3, 30) // 起動まで最大~300s
		mainContainer.ReadinessProbe = httpProbe(healthPath, 10, 3, 3)
		mainContainer.LivenessProbe = httpProbe(healthPath, 20, 5, 3)
	}

	return corev1.PodSpec{
		ImagePullSecrets:          m.Spec.ImagePullSecrets,
		SecurityContext:           nonRootPodSecurityContext(misskeyUID),
		TopologySpreadConstraints: spread,
		NodeSelector:              comp.NodeSelector,
		Tolerations:               comp.Tolerations,
		InitContainers:            misskeyInitContainers(m, p),
		Containers:                []corev1.Container{mainContainer},
		Volumes:                   misskeyVolumes(m),
	}
}

// misskeyInitContainers: built/をwritable emptyDirへコピー + default.ymlのプレースホルダ展開
// app/worker/migration Jobで共用
func misskeyInitContainers(m *misskeyv1alpha1.Misskey, p plan) []corev1.Container {
	return []corev1.Container{
		{
			// built/を書込可能なemptyDirにコピー。compile-config等が書くため
			Name:            "prepare-built",
			Image:           m.Spec.Image,
			Command:         []string{"sh", "-c", "cp -r /misskey/built/. /tmp/built/"},
			SecurityContext: restrictedContainerSecurityContext(),
			VolumeMounts:    []corev1.VolumeMount{{Name: "built-volume", MountPath: "/tmp/built"}},
		},
		{
			// default.ymlの${...}をリテラル置換で展開(シェル・正規表現なし)
			Name:            "render-config",
			Image:           m.Spec.Image,
			Command:         []string{"node", "-e", renderConfigScript},
			Env:             renderInitEnv(p),
			SecurityContext: restrictedContainerSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config-tpl", MountPath: "/tpl"},
				{Name: "config-rendered", MountPath: "/shared"},
			},
		},
	}
}

// misskeyVolumes: config template / rendered / built の3volume
func misskeyVolumes(m *misskeyv1alpha1.Misskey) []corev1.Volume {
	return []corev1.Volume{
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
	}
}

// misskeyConfigMount: default.ymlをread-onlyで、built/をwritableでマウント(main container用)
func misskeyConfigMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "config-rendered", MountPath: "/misskey/.config/default.yml", SubPath: "default.yml", ReadOnly: true},
		{Name: "built-volume", MountPath: "/misskey/built"},
	}
}
