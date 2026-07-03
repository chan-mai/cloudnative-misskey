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

import misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"

// upstream misskey/misskeyイメージの既定。fork対応で全てspec.runtimeから上書き可
const (
	defaultMisskeyUID = 991
	defaultConfigPath = "/misskey/.config/default.yml"
	defaultBuiltPath  = "/misskey/built"
	defaultHealthPath = "/api/server-info"
)

// runtimeUID: spec.runtime.runAsUser、未指定なら991
func runtimeUID(m *misskeyv1alpha1.Misskey) int64 {
	if u := m.Spec.Runtime.RunAsUser; u != nil {
		return *u
	}
	return defaultMisskeyUID
}

// runtimeStartCommand: app/workerコマンド、既定`pnpm run start`
func runtimeStartCommand(m *misskeyv1alpha1.Misskey) []string {
	if c := m.Spec.Runtime.StartCommand; len(c) > 0 {
		return c
	}
	return []string{"pnpm", "run", "start"}
}

// runtimeMigrateCommand: migration Jobコマンド、既定`pnpm run migrate`
func runtimeMigrateCommand(m *misskeyv1alpha1.Misskey) []string {
	if c := m.Spec.Runtime.MigrateCommand; len(c) > 0 {
		return c
	}
	return []string{"pnpm", "run", "migrate"}
}

// runtimeHealthPath: probeパス、既定`/api/server-info`
func runtimeHealthPath(m *misskeyv1alpha1.Misskey) string {
	return stringOr(m.Spec.Runtime.HealthPath, defaultHealthPath)
}

// runtimeConfigPath: default.ymlのmount先、既定`/misskey/.config/default.yml`
func runtimeConfigPath(m *misskeyv1alpha1.Misskey) string {
	return stringOr(m.Spec.Runtime.ConfigPath, defaultConfigPath)
}

// runtimeBuiltPath: built/コピー先、既定`/misskey/built`。空文字でコピー無効
func runtimeBuiltPath(m *misskeyv1alpha1.Misskey) string {
	if p := m.Spec.Runtime.BuiltPath; p != nil {
		return *p
	}
	return defaultBuiltPath
}
