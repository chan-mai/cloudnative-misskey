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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

func newMisskey() *misskeyv1alpha1.Misskey {
	return &misskeyv1alpha1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "ns"},
		Spec: misskeyv1alpha1.MisskeySpec{
			URL:   "https://misskey.example.com/",
			Image: "misskey/misskey:2026.6.0",
		},
	}
}

func TestHostFromURL(t *testing.T) {
	cases := map[string]string{
		"https://misskey.example.com/":     "misskey.example.com",
		"http://mk.example.org":            "mk.example.org",
		"https://mk.example.org:8443/path": "mk.example.org",
		"mk.example.org/foo":               "mk.example.org",
		"":                                 "",
	}
	for in, want := range cases {
		if got := hostFromURL(in); got != want {
			t.Errorf("hostFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeIndex(t *testing.T) {
	cases := map[string]string{
		"misskey.example.com": "misskey-example-com",
		"MK_1.io":             "MK_1-io",
		"..a..":               "a",
		"":                    "misskey",
	}
	for in, want := range cases {
		if got := sanitizeIndex(in); got != want {
			t.Errorf("sanitizeIndex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveManaged(t *testing.T) {
	p := resolve(newMisskey())

	if !p.dbManaged || p.dbHost != "example-db-rw" || p.dbName != "misskey" || p.dbUser != "misskey" {
		t.Errorf("managed db resolved wrong: %+v", p)
	}
	if p.dbPassSel.Name != "example-db-app" || p.dbPassSel.Key != "password" {
		t.Errorf("db password selector wrong: %+v", p.dbPassSel)
	}
	if !p.redisManaged || p.redisHost != "example-redis" {
		t.Errorf("managed redis resolved wrong: %+v", p)
	}
	if !p.meiliEnabled || !p.meiliManaged || p.meiliHost != "example-meilisearch" {
		t.Errorf("managed meili resolved wrong: %+v", p)
	}
	if p.meiliIndex != "misskey-example-com" || p.meiliScope != "local" {
		t.Errorf("meili index/scope wrong: %q %q", p.meiliIndex, p.meiliScope)
	}
	if p.meiliKeySel.Name != "example-meilisearch" || p.meiliKeySel.Key != meiliMasterKeyID {
		t.Errorf("meili key selector wrong: %+v", p.meiliKeySel)
	}
	if p.ingressHost != "misskey.example.com" {
		t.Errorf("ingress host wrong: %q", p.ingressHost)
	}
}

func TestResolveExternal(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.External = &misskeyv1alpha1.ExternalPostgres{
		Host: "pg.db.svc", Port: 6543, Database: "d", User: "u",
		PasswordSecret: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw",
		},
	}
	m.Spec.Redis.External = &misskeyv1alpha1.ExternalRedis{Host: "redis.svc"}
	m.Spec.Search.Meilisearch.External = &misskeyv1alpha1.ExternalMeilisearch{
		Host: "meili.svc", SSL: true,
		APIKeySecret: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "meilisec"}, Key: "k",
		},
	}

	p := resolve(m)
	if p.dbManaged || p.dbHost != "pg.db.svc" || p.dbPort != 6543 || p.dbUser != "u" {
		t.Errorf("external db resolved wrong: %+v", p)
	}
	if p.dbPassSel.Name != "pgsec" || p.dbPassSel.Key != "pw" {
		t.Errorf("external db pass selector wrong: %+v", p.dbPassSel)
	}
	if p.redisManaged || p.redisHost != "redis.svc" {
		t.Errorf("external redis resolved wrong: %+v", p)
	}
	if p.meiliManaged || p.meiliHost != "meili.svc" || !p.meiliSSL {
		t.Errorf("external meili resolved wrong: %+v", p)
	}
	if p.meiliKeySel.Name != "meilisec" {
		t.Errorf("external meili key selector wrong: %+v", p.meiliKeySel)
	}
}

func TestResolveProviderSQLLike(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1alpha1.SearchSQLLike
	p := resolve(m)
	if p.meiliEnabled {
		t.Errorf("sqlLike should not enable meilisearch")
	}
}

func TestResolveSetupPassword(t *testing.T) {
	// managed(生成)
	m := newMisskey()
	m.Spec.SetupPassword = &misskeyv1alpha1.SetupPasswordSpec{}
	p := resolve(m)
	if !p.setupEnabled || !p.setupManaged || p.setupSel.Name != "example-setup" || p.setupSel.Key != setupPasswordID {
		t.Errorf("managed setup password wrong: %+v", p.setupSel)
	}

	// external secretRefの場合
	m2 := newMisskey()
	m2.Spec.SetupPassword = &misskeyv1alpha1.SetupPasswordSpec{
		SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "mysetup"}, Key: "SETUP",
		},
	}
	p2 := resolve(m2)
	if !p2.setupEnabled || p2.setupManaged || p2.setupSel.Name != "mysetup" {
		t.Errorf("external setup password wrong: %+v", p2.setupSel)
	}
}

func TestRenderDefaultYMLMeilisearch(t *testing.T) {
	m := newMisskey()
	m.Spec.SetupPassword = &misskeyv1alpha1.SetupPasswordSpec{}
	m.Spec.ExtraConfig = "maxFileSize: 100"
	out := renderDefaultYML(m, resolve(m))

	mustContain := []string{
		"url: https://misskey.example.com/",
		"host: example-db-rw",
		"user: misskey",
		"pass: ${DB_PASSWORD}",
		"host: example-redis",
		"provider: meilisearch",
		"host: example-meilisearch",
		"apiKey: ${MEILI_KEY}",
		"index: misskey-example-com",
		"setupPassword: ${SETUP_PASSWORD}",
		"id: 'aidx'",
		"maxFileSize: 100",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("default.yml missing %q\n---\n%s", s, out)
		}
	}
	// シークレットはプレースホルダのまま。実値は描画されない
	if strings.Contains(out, "MEILI_MASTER_KEY") {
		t.Errorf("default.yml unexpectedly contains a secret key name")
	}
}

func TestRenderDefaultYMLSQLLike(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1alpha1.SearchSQLPgroonga
	out := renderDefaultYML(m, resolve(m))
	if !strings.Contains(out, "provider: sqlPgroonga") {
		t.Errorf("expected sqlPgroonga provider")
	}
	if strings.Contains(out, "meilisearch:") || strings.Contains(out, "MEILI_KEY") {
		t.Errorf("non-meili provider must not emit a meilisearch block:\n%s", out)
	}
}

func TestRenderCaddyfileDefaults(t *testing.T) {
	out := renderCaddyfile(newMisskey())

	mustContain := []string{
		"reverse_proxy example-app:3000",
		"health_uri /api/server-info",
		"@api path /api/*",
		`respond "" {err.status_code}`,
		"copy_response 200", // メンテナンスの既定ステータス
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("Caddyfile missing %q\n---\n%s", s, out)
		}
	}
	// Fix5: X-Forwarded-Protoを{scheme}で上書きしない
	if strings.Contains(out, "X-Forwarded-Proto") {
		t.Errorf("Caddyfile should not set X-Forwarded-Proto:\n%s", out)
	}
	// Fix4: ソースヘッダ未設定ならクライアントIPを上書きしない
	if strings.Contains(out, "X-Real-IP") {
		t.Errorf("Caddyfile should not set X-Real-IP by default:\n%s", out)
	}
}

func TestRenderCaddyfileClientIPAndStatus(t *testing.T) {
	m := newMisskey()
	m.Spec.Proxy.ClientIPHeader = "CF-Connecting-IP"
	m.Spec.Proxy.Maintenance.StatusCode = int32Ptr(503)
	out := renderCaddyfile(m)

	if !strings.Contains(out, "header_up X-Real-IP {header.CF-Connecting-IP}") {
		t.Errorf("expected CF-Connecting-IP client IP header:\n%s", out)
	}
	if !strings.Contains(out, "copy_response 503") {
		t.Errorf("expected configurable maintenance status 503:\n%s", out)
	}
}

func TestMaintenanceHTMLReload(t *testing.T) {
	if !strings.Contains(defaultMaintenanceHTML(15), "location.reload") ||
		!strings.Contains(defaultMaintenanceHTML(15), "15000") {
		t.Errorf("reloadSeconds>0 must embed a reload script")
	}
	if strings.Contains(defaultMaintenanceHTML(0), "location.reload") {
		t.Errorf("reloadSeconds=0 must not embed a reload script")
	}

	m := newMisskey()
	m.Spec.Proxy.Maintenance.HTML = "<h1>custom</h1>"
	if maintenanceHTMLContent(m) != "<h1>custom</h1>" {
		t.Errorf("custom maintenance html should win over the default")
	}
}

func TestRenderConfigScriptIsLiteral(t *testing.T) {
	// Fix1: render段はsedでなくリテラル置換(split/join)を使い、シークレット値中の任意文字が壊れ・インジェクションを起こさないようにする
	if !strings.Contains(renderConfigScript, ".split(") || !strings.Contains(renderConfigScript, ".join(") {
		t.Errorf("render script must use literal split/join replacement")
	}
	if strings.Contains(renderConfigScript, "sed") {
		t.Errorf("render script must not shell out to sed")
	}
}

func TestChecksumAnnotation(t *testing.T) {
	a := checksumAnnotation("default.yml body")
	b := checksumAnnotation("default.yml body")
	c := checksumAnnotation("changed body")

	if a[configChecksumAnnotation] == "" {
		t.Fatalf("checksum annotation key %q missing", configChecksumAnnotation)
	}
	if a[configChecksumAnnotation] != b[configChecksumAnnotation] {
		t.Errorf("same input must yield the same checksum")
	}
	if a[configChecksumAnnotation] == c[configChecksumAnnotation] {
		t.Errorf("different input must change the checksum (else pods never roll)")
	}
}

func TestTenantOf(t *testing.T) {
	m := newMisskey()
	// 未設定はnamespaceにfallback
	if got := tenantOf(m); got != "ns" {
		t.Errorf("fallback: got %q, want ns", got)
	}
	// 明示時はその値
	m.Spec.Tenant = "acme-corp"
	if got := tenantOf(m); got != "acme-corp" {
		t.Errorf("explicit: got %q, want acme-corp", got)
	}
}

func TestLabelsForTenant(t *testing.T) {
	const key = "cloudnative-misskey.dev/tenant"
	m := newMisskey()
	m.Spec.Tenant = "acme-corp"
	if got := labelsFor(m, roleApp)[key]; got != "acme-corp" {
		t.Errorf("tenant label: got %q, want acme-corp", got)
	}
	// selectorForにtenantを含めない(不変selector維持)
	if _, ok := selectorFor(m, roleApp)[key]; ok {
		t.Error("selectorForにtenantが混入している")
	}
}

func TestRuntimeDefaults(t *testing.T) {
	m := newMisskey()
	if runtimeUID(m) != 991 {
		t.Errorf("uid default: %d", runtimeUID(m))
	}
	if got := strings.Join(runtimeStartCommand(m), " "); got != "pnpm run start" {
		t.Errorf("start: %q", got)
	}
	if got := strings.Join(runtimeMigrateCommand(m), " "); got != "pnpm run migrate" {
		t.Errorf("migrate: %q", got)
	}
	if runtimeHealthPath(m) != "/api/server-info" {
		t.Errorf("health: %q", runtimeHealthPath(m))
	}
	if runtimeConfigPath(m) != "/misskey/.config/default.yml" {
		t.Errorf("config: %q", runtimeConfigPath(m))
	}
	if runtimeBuiltPath(m) != "/misskey/built" {
		t.Errorf("built: %q", runtimeBuiltPath(m))
	}
}

func TestRuntimeOverrides(t *testing.T) {
	uid := int64(1000)
	empty := ""
	m := newMisskey()
	m.Spec.Runtime = misskeyv1alpha1.RuntimeSpec{
		RunAsUser:      &uid,
		StartCommand:   []string{"node", "start.js"},
		MigrateCommand: []string{"node", "migrate.js"},
		HealthPath:     "/healthz",
		ConfigPath:     "/app/config.yml",
		BuiltPath:      &empty, // 空でコピー無効
	}
	if runtimeUID(m) != 1000 {
		t.Errorf("uid override: %d", runtimeUID(m))
	}
	if got := strings.Join(runtimeStartCommand(m), " "); got != "node start.js" {
		t.Errorf("start override: %q", got)
	}
	if got := strings.Join(runtimeMigrateCommand(m), " "); got != "node migrate.js" {
		t.Errorf("migrate override: %q", got)
	}
	if runtimeHealthPath(m) != "/healthz" {
		t.Errorf("health override: %q", runtimeHealthPath(m))
	}
	if runtimeConfigPath(m) != "/app/config.yml" {
		t.Errorf("config override: %q", runtimeConfigPath(m))
	}
	if runtimeBuiltPath(m) != "" {
		t.Errorf("built override should be empty(disabled): %q", runtimeBuiltPath(m))
	}
}

func hasMount(mounts []corev1.VolumeMount, path string) bool {
	for _, mnt := range mounts {
		if mnt.MountPath == path {
			return true
		}
	}
	return false
}

func hasContainer(cs []corev1.Container, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestBuildPodSpecRuntime(t *testing.T) {
	m := newMisskey()
	p := resolve(m)

	// default: uid 991、start command、config/builtマウント、prepare-built init有り
	spec := buildMisskeyPodSpec(m, p, roleApp, m.Spec.App)
	if spec.SecurityContext.RunAsUser == nil || *spec.SecurityContext.RunAsUser != 991 {
		t.Error("default uid != 991")
	}
	c := spec.Containers[0]
	if strings.Join(c.Command, " ") != "pnpm run start" {
		t.Errorf("default command: %v", c.Command)
	}
	if !hasMount(c.VolumeMounts, "/misskey/.config/default.yml") {
		t.Error("config mount欠落")
	}
	if !hasMount(c.VolumeMounts, "/misskey/built") {
		t.Error("built mount欠落")
	}
	if !hasContainer(spec.InitContainers, "prepare-built") {
		t.Error("prepare-built init欠落")
	}

	// builtPath="" → built mount/prepare-built無し
	empty := ""
	m.Spec.Runtime.BuiltPath = &empty
	spec = buildMisskeyPodSpec(m, p, roleApp, m.Spec.App)
	if hasMount(spec.Containers[0].VolumeMounts, "/misskey/built") {
		t.Error("builtPath=空でbuilt mountが残っている")
	}
	if hasContainer(spec.InitContainers, "prepare-built") {
		t.Error("builtPath=空でprepare-buildが残っている")
	}
}
