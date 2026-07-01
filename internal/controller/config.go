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
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// renderDefaultYML builds the Misskey .config/default.yml. Secrets are left as
// ${PLACEHOLDER} tokens; an init container substitutes them at pod start so no
// secret value ever lands in a ConfigMap.
func renderDefaultYML(m *misskeyv1alpha1.Misskey, p plan) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Managed by cloud-native-misskey. Do not edit by hand.\n")
	w("url: %s\n", m.Spec.URL)
	w("port: %d\n\n", misskeyPort)

	if p.setupEnabled {
		w("setupPassword: ${SETUP_PASSWORD}\n\n")
	}

	w("db:\n")
	w("  host: %s\n", p.dbHost)
	w("  port: %d\n", p.dbPort)
	w("  db: %s\n", p.dbName)
	w("  user: %s\n", p.dbUser)
	w("  pass: ${DB_PASSWORD}\n")
	w("dbReplications: false\n\n")

	w("redis:\n")
	w("  host: %s\n", p.redisHost)
	w("  port: %d\n", p.redisPort)
	if p.redisPassSel != nil {
		w("  pass: ${REDIS_PASSWORD}\n")
	}
	w("\n")

	w("fulltextSearch:\n")
	w("  provider: %s\n", p.provider)
	if p.meiliEnabled {
		w("\nmeilisearch:\n")
		w("  host: %s\n", p.meiliHost)
		w("  port: %d\n", p.meiliPort)
		w("  apiKey: ${MEILI_KEY}\n")
		w("  ssl: %s\n", strconv.FormatBool(p.meiliSSL))
		w("  index: %s\n", p.meiliIndex)
		w("  scope: %s\n", p.meiliScope)
	}
	w("\n")

	w("id: '%s'\n", stringOr(m.Spec.IDGenerationMethod, "aidx"))
	w("proxyRemoteFiles: true\n")
	w("signToActivityPubGet: true\n")

	if strings.TrimSpace(m.Spec.ExtraConfig) != "" {
		w("\n# --- extraConfig ---\n%s\n", strings.TrimRight(m.Spec.ExtraConfig, "\n"))
	}
	return b.String()
}

// renderCaddyfile builds the reverse-proxy Caddyfile fronting the app.
func renderCaddyfile(m *misskeyv1alpha1.Misskey) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s) }

	w(fmt.Sprintf(":%d {\n", proxyPort))
	w("\tencode gzip\n")
	w("\theader /assets Cache-Control \"public, max-age=31536000, immutable\"\n\n")
	w(fmt.Sprintf("\treverse_proxy %s:%d {\n", nameApp(m), misskeyPort))
	w("\t\theader_up X-Real-IP {header.CF-Connecting-IP}\n")
	w("\t\theader_up X-Forwarded-For {header.CF-Connecting-IP}\n")
	w("\t\theader_up X-Forwarded-Proto {scheme}\n")
	w("\t\theader_up X-Forwarded-Host {host}\n\n")
	w("\t\thealth_uri /api/server-info\n")
	w("\t\thealth_interval 10s\n")
	w("\t\thealth_timeout 2s\n")
	w("\t\thealth_status 200\n")
	w("\t}\n")

	if boolOr(m.Spec.Proxy.Maintenance.Enabled, true) {
		w("\n\thandle_errors {\n")
		w("\t\t@backend_down `{err.status_code} in [500, 501, 502, 503, 504, 522]`\n")
		w("\t\thandle @backend_down {\n")
		w(fmt.Sprintf("\t\t\treverse_proxy %s:%d {\n", nameMaintenance(m), proxyPort))
		w("\t\t\t\thandle_response {\n")
		w("\t\t\t\t\tcopy_response_headers\n")
		w("\t\t\t\t\tcopy_response 503\n")
		w("\t\t\t\t}\n")
		w("\t\t\t}\n")
		w("\t\t}\n")
		w("\t}\n")
	}
	w("}\n")
	return b.String()
}

// renderMaintenanceCaddyfile builds the static maintenance server config.
func renderMaintenanceCaddyfile() string {
	return fmt.Sprintf(":%d {\n\troot * /usr/share/caddy\n\ttry_files /maintenance.html\n\tfile_server\n\theader Cache-Control \"no-store\"\n}\n", proxyPort)
}

// defaultMaintenanceHTML is served when spec.proxy.maintenance.html is empty.
const defaultMaintenanceHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Under maintenance</title>
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#f5f5f7;color:#333}main{text-align:center}</style>
</head>
<body><main><h1>Under maintenance</h1><p>This server is temporarily unavailable. Please try again shortly.</p></main></body>
</html>
`

// reconcileConfigMaps creates/updates the config ConfigMap (default.yml +
// Caddyfiles) and, when the proxy is enabled, the maintenance HTML ConfigMap.
func (r *MisskeyReconciler) reconcileConfigMaps(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameConfig(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, cm, func() error {
		cm.Labels = labelsFor(m, "config")
		cm.Data = map[string]string{
			"default.yml": renderDefaultYML(m, p),
		}
		if boolOr(m.Spec.Proxy.Enabled, true) {
			cm.Data["Caddyfile"] = renderCaddyfile(m)
			cm.Data["maintenance.Caddyfile"] = renderMaintenanceCaddyfile()
		}
		return nil
	}); err != nil {
		return err
	}

	if !boolOr(m.Spec.Proxy.Enabled, true) || !boolOr(m.Spec.Proxy.Maintenance.Enabled, true) {
		return nil
	}
	html := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameMaintenanceHTML(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, html, func() error {
		html.Labels = labelsFor(m, "maintenance")
		html.Data = map[string]string{
			"maintenance.html": stringOr(m.Spec.Proxy.Maintenance.HTML, defaultMaintenanceHTML),
		}
		return nil
	})
}

// reconcileSetupSecret ensures the operator-managed setup-password Secret
// exists. It is created only when spec.setupPassword is present without a
// secretRef. The generated password is never overwritten once set.
func (r *MisskeyReconciler) reconcileSetupSecret(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameSetup(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, "setup")
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data[setupPasswordID]; !ok {
			pw, err := randomHex(16)
			if err != nil {
				return err
			}
			secret.Data[setupPasswordID] = []byte(pw)
		}
		return nil
	})
}

// apply is a thin CreateOrUpdate wrapper that also stamps the controller owner
// reference so children are garbage-collected with the Misskey object.
func (r *MisskeyReconciler) apply(ctx context.Context, m *misskeyv1alpha1.Misskey, obj client.Object, mutate func() error) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(m, obj, r.Scheme)
	})
	return err
}
