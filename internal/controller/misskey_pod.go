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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

const (
	roleApp    = "app"
	roleWorker = "worker"
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
	// default + 各role redisのpassword。managed(requirepass)/external問わずpassSelがあれば注入
	if p.redisDefault.passSel != nil {
		env = append(env, secretEnv(p.redisDefault.passEnv, *p.redisDefault.passSel))
	}
	for _, rd := range redisRoleDescs {
		if ep, ok := p.redisRoles[rd.key]; ok && ep.passSel != nil {
			env = append(env, secretEnv(ep.passEnv, *ep.passSel))
		}
	}
	if p.setupEnabled {
		env = append(env, secretEnv("SETUP_PASSWORD", p.setupSel))
	}
	return env
}

// default.ymlの${...}シークレットプレースホルダをリテラル文字列置換で展開するNode.jsプログラム
// String.split(literal).join(value)により、正規表現・シェル解釈(sed時代のインジェクション経路)を回避
// 値はJSON.stringifyでquoteして埋め込む。JSON文字列はYAML double-quoted scalarとして常に妥当なため、
// 改行・#・引用符等を含む外部Secret値でもYAML破損やキー注入が起きない
// MisskeyイメージにNode同梱のため追加のツールイメージは不要
const renderConfigScript = `const fs = require('fs');
let s = fs.readFileSync('/tpl/default.yml', 'utf8');
for (const k of ['DB_PASSWORD', 'MEILI_KEY', 'REDIS_PASSWORD', 'REDIS_PASSWORD_JOBQUEUE', 'REDIS_PASSWORD_PUBSUB', 'REDIS_PASSWORD_TIMELINES', 'REDIS_PASSWORD_REACTIONS', 'SETUP_PASSWORD']) {
  const v = process.env[k];
  if (v !== undefined) s = s.split('${' + k + '}').join(JSON.stringify(v));
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
func buildMisskeyPodSpec(m *misskeyv1beta1.Misskey, p plan, role string, comp misskeyv1beta1.ComponentSpec) corev1.PodSpec {
	env := []corev1.EnvVar{
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
		// migrationはmigration Jobに一本化。startのみ(fork向けにruntime.startCommandで上書き可)
		Command:         runtimeStartCommand(m),
		SecurityContext: restrictedContainerSecurityContext(),
		Resources:       res,
		Env:             env,
		Ports:           ports,
		VolumeMounts:    misskeyConfigMounts(m),
	}
	// appはHTTPを提供するため、readinessと再起動をヘルスエンドポイントで制御
	// startupProbeが遅い初回起動を吸収。workerはキュー専用でprobeなし
	if role == roleApp {
		healthPath := runtimeHealthPath(m)
		mainContainer.StartupProbe = httpProbe(healthPath, 10, 3, 30) // 起動まで最大~300s
		mainContainer.ReadinessProbe = httpProbe(healthPath, 10, 3, 3)
		mainContainer.LivenessProbe = httpProbe(healthPath, 20, 5, 3)
	}

	return corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		ImagePullSecrets:             m.Spec.ImagePullSecrets,
		SecurityContext:              nonRootPodSecurityContext(runtimeUID(m)),
		TopologySpreadConstraints:    spread,
		NodeSelector:                 comp.NodeSelector,
		Tolerations:                  comp.Tolerations,
		InitContainers:               misskeyInitContainers(m, p),
		Containers:                   []corev1.Container{mainContainer},
		Volumes:                      misskeyVolumes(m, nameConfig(m)),
	}
}

// misskeyInitContainers: built/をwritable emptyDirへコピー + default.ymlのプレースホルダ展開
// app/worker/migration Jobで共用。builtPathが空ならコピーinitを省く
func misskeyInitContainers(m *misskeyv1beta1.Misskey, p plan) []corev1.Container {
	var inits []corev1.Container
	if bp := runtimeBuiltPath(m); bp != "" {
		// built/を書込可能なemptyDirにコピー。compile-config等が書くため
		inits = append(inits, corev1.Container{
			Name:            "prepare-built",
			Image:           m.Spec.Image,
			Command:         []string{"cp", "-r", bp + "/.", "/tmp/built/"},
			SecurityContext: restrictedContainerSecurityContext(),
			VolumeMounts:    []corev1.VolumeMount{{Name: "built-volume", MountPath: "/tmp/built"}},
		})
	}
	// default.ymlの${...}をリテラル置換で展開(シェル・正規表現なし)
	inits = append(inits, corev1.Container{
		Name:            "render-config",
		Image:           m.Spec.Image,
		Command:         []string{"node", "-e", renderConfigScript},
		Env:             renderInitEnv(p),
		SecurityContext: restrictedContainerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config-tpl", MountPath: "/tpl"},
			{Name: "config-rendered", MountPath: "/shared"},
			tmpMount(),
		},
	})
	return inits
}

// misskeyVolumes: config template(cfg ConfigMap) / rendered、builtPath有効時はbuilt emptyDir
// cfgでtemplate元ConfigMapを切替(app/worker=nameConfig、migration=nameMigrateConfig)
func misskeyVolumes(m *misskeyv1beta1.Misskey, cfg string) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name: "config-tpl",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cfg},
				},
			},
		},
		{Name: "config-rendered", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		tmpVolume(),
	}
	if runtimeBuiltPath(m) != "" {
		vols = append(vols, corev1.Volume{Name: "built-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return vols
}

// misskeyConfigMounts: default.ymlをread-only、builtPath有効時はwritableでマウント(main container用)
func misskeyConfigMounts(m *misskeyv1beta1.Misskey) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config-rendered", MountPath: runtimeConfigPath(m), SubPath: "default.yml", ReadOnly: true},
		tmpMount(),
	}
	if bp := runtimeBuiltPath(m); bp != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "built-volume", MountPath: bp})
	}
	return mounts
}
