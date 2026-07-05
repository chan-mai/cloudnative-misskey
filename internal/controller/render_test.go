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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

func TestPoolerIgnoreStartupParameters(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
	u := buildPooler(m, nameDBPoolerRW(m), "rw")
	params, _, _ := unstructured.NestedStringMap(u.Object, "spec", "pgbouncer", "parameters")
	// transaction pooling下でMisskeyのstatement_timeoutを無視しないと接続失敗する回帰防止
	if !strings.Contains(params["ignore_startup_parameters"], "statement_timeout") {
		t.Errorf("pooler must ignore statement_timeout: %v", params)
	}
}

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
	if !p.redisManaged || p.redisDefault.host != "example-redis" {
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
	if p.redisManaged || p.redisDefault.host != "redis.svc" {
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

func TestReadOffloadAuto(t *testing.T) {
	// instances>=2でread offload自動オン、slaveは-roサービス(pooler無し)
	m := newMisskey()
	m.Spec.Postgres.Instances = 2
	p := resolve(m)
	if p.dbHost != "example-db-rw" {
		t.Errorf("write host should stay -rw without pooler: %q", p.dbHost)
	}
	if !p.dbReplications || len(p.dbSlaves) != 1 || p.dbSlaves[0].host != "example-db-ro" {
		t.Errorf("read offload not wired to -ro: replications=%v slaves=%+v", p.dbReplications, p.dbSlaves)
	}
	if p.dbSlaves[0].db != "misskey" || p.dbSlaves[0].user != "misskey" || p.dbSlaves[0].port != 5432 {
		t.Errorf("dbSlave endpoint wrong: %+v", p.dbSlaves[0])
	}
}

func TestReadOffloadSingleInstance(t *testing.T) {
	// instances=1(既定)はreplica不在。read offloadしない
	p := resolve(newMisskey())
	if p.dbReplications || len(p.dbSlaves) != 0 {
		t.Errorf("single instance must not offload reads: %+v", p)
	}
}

func TestReadOffloadOptOut(t *testing.T) {
	// instances>=2でもreadOffload:falseで明示オプトアウト
	m := newMisskey()
	m.Spec.Postgres.Instances = 2
	m.Spec.Postgres.ReadOffload = boolPtr(false)
	p := resolve(m)
	if p.dbReplications || len(p.dbSlaves) != 0 {
		t.Errorf("readOffload=false must disable offload: %+v", p)
	}
}

func TestResolvePoolerHosts(t *testing.T) {
	// pooler有効: writeはrwプーラー、readはroプーラーへ
	m := newMisskey()
	m.Spec.Postgres.Instances = 2
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
	p := resolve(m)
	if p.dbHost != "example-db-pooler-rw" {
		t.Errorf("write host should be rw pooler: %q", p.dbHost)
	}
	if !p.dbReplications || len(p.dbSlaves) != 1 || p.dbSlaves[0].host != "example-db-pooler-ro" {
		t.Errorf("read host should be ro pooler: %+v", p.dbSlaves)
	}
}

func TestResolvePoolerNoOffload(t *testing.T) {
	// pooler有効・instances=1: writeはrwプーラー、read offloadはしない(roプーラー不要)
	m := newMisskey()
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
	p := resolve(m)
	if p.dbHost != "example-db-pooler-rw" {
		t.Errorf("write host should be rw pooler: %q", p.dbHost)
	}
	if p.dbReplications || len(p.dbSlaves) != 0 {
		t.Errorf("no replica → no offload: %+v", p)
	}
}

func TestRenderDefaultYMLReadOffload(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Instances = 2
	out := renderDefaultYML(m, resolve(m))
	for _, s := range []string{"dbReplications: true", "dbSlaves:", "host: example-db-ro", "pass: ${DB_PASSWORD}"} {
		if !strings.Contains(out, s) {
			t.Errorf("read-offload default.yml missing %q\n---\n%s", s, out)
		}
	}
	// 単一インスタンスはdbReplications: falseのまま
	if strings.Contains(renderDefaultYML(newMisskey(), resolve(newMisskey())), "dbReplications: true") {
		t.Error("single instance must render dbReplications: false")
	}
}

func TestMigratePlanPrimaryDirect(t *testing.T) {
	// pooler+offload構成でも、migrationはprimary(-rw)直結・no-replication
	m := newMisskey()
	m.Spec.Postgres.Instances = 2
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
	mp := migratePlan(m, resolve(m))
	if mp.dbHost != "example-db-rw" {
		t.Errorf("migration must bypass pooler to -rw: %q", mp.dbHost)
	}
	if mp.dbReplications || len(mp.dbSlaves) != 0 {
		t.Errorf("migration must not use replicas: %+v", mp)
	}
	out := renderDefaultYML(m, mp)
	if !strings.Contains(out, "host: example-db-rw") || !strings.Contains(out, "dbReplications: false") {
		t.Errorf("migrate config not primary-direct:\n%s", out)
	}
}

func TestBuildPooler(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{
		Instances:  3,
		Parameters: map[string]string{"default_pool_size": "50"},
	}
	pooler := buildPooler(m, nameDBPoolerRW(m), "rw")

	if pooler.GetName() != "example-db-pooler-rw" || pooler.GetKind() != "Pooler" {
		t.Errorf("pooler identity wrong: %s/%s", pooler.GetKind(), pooler.GetName())
	}
	spec := pooler.Object["spec"].(map[string]any)
	if spec["type"] != "rw" {
		t.Errorf("type: %v", spec["type"])
	}
	if spec["instances"] != int64(3) {
		t.Errorf("instances: %v", spec["instances"])
	}
	if cl := spec["cluster"].(map[string]any); cl["name"] != "example-db" {
		t.Errorf("cluster name: %v", cl["name"])
	}
	pgb := spec["pgbouncer"].(map[string]any)
	if pgb["poolMode"] != "transaction" {
		t.Errorf("poolMode default: %v", pgb["poolMode"])
	}
	params := pgb["parameters"].(map[string]any)
	if params["max_client_conn"] != "1000" || params["default_pool_size"] != "50" {
		t.Errorf("params merge wrong: %+v", params)
	}
	// pooler podは既存NetworkPolicyがintra扱いするためinstance/componentラベル必須
	labels := spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["app.kubernetes.io/instance"] != "example" || labels["app.kubernetes.io/component"] != "postgres-pooler" {
		t.Errorf("pooler pod labels missing instance/component: %+v", labels)
	}
}

func TestPoolerHelpers(t *testing.T) {
	m := newMisskey()
	if poolerEnabled(m) {
		t.Error("pooler未指定はdisabled")
	}
	// ポインタ存在=有効(内側Enabledは廃止)
	m.Spec.Postgres.Pooler = &misskeyv1alpha1.PostgresPooler{}
	if !poolerEnabled(m) {
		t.Error("pooler block存在でenabled")
	}
}

func TestRedisHAAuth(t *testing.T) {
	// HA redis: requirepass secret参照 + passEnv
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{}
	ep := resolve(m).redisDefault
	if ep.passSel == nil || ep.passSel.Name != "example-redis-auth" || ep.passSel.Key != "password" {
		t.Errorf("HA redis must carry requirepass secret ref: %+v", ep.passSel)
	}
	if ep.passEnv != "REDIS_PASSWORD" {
		t.Errorf("default HA passEnv: %q", ep.passEnv)
	}
	// standalone managed: 認証なし(NP保護)
	if resolve(newMisskey()).redisDefault.passSel != nil {
		t.Error("standalone managed redis must not have auth")
	}
	// role HA: role別passEnv
	m2 := newMisskey()
	m2.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{JobQueue: &misskeyv1alpha1.RedisRole{HA: &misskeyv1alpha1.RedisHA{}}}
	epr := resolve(m2).redisRoles["jobQueue"]
	if epr.passSel == nil || epr.passEnv != "REDIS_PASSWORD_JOBQUEUE" {
		t.Errorf("role HA auth wrong: sel=%+v env=%q", epr.passSel, epr.passEnv)
	}
	// config: HAで pass + sentinelPassword プレースホルダ出力(OT sentinelもrequirepass)
	out := renderDefaultYML(m, resolve(m))
	for _, s := range []string{"pass: ${REDIS_PASSWORD}", "sentinelPassword: ${REDIS_PASSWORD}"} {
		if !strings.Contains(out, s) {
			t.Errorf("HA redis config must emit %q:\n%s", s, out)
		}
	}
	// KEDA TriggerAuth: sentinel経路はpassword + sentinelPassword両方
	ta := buildTriggerAuth(m, "example-worker", ep)
	params := map[string]bool{}
	for _, ref := range ta.Object["spec"].(map[string]any)["secretTargetRef"].([]any) {
		params[ref.(map[string]any)["parameter"].(string)] = true
	}
	if !params["password"] || !params["sentinelPassword"] {
		t.Errorf("sentinel TriggerAuth must include password + sentinelPassword: %+v", params)
	}
}

func TestResolveRedisRoleFallback(t *testing.T) {
	// roles未指定 → redisRolesは空、configはredisForXxxを出さない
	p := resolve(newMisskey())
	if len(p.redisRoles) != 0 {
		t.Errorf("no roles must yield empty redisRoles: %+v", p.redisRoles)
	}
	out := renderDefaultYML(newMisskey(), p)
	for _, s := range []string{"redisForJobQueue", "redisForPubsub", "redisForTimelines", "redisForReactions"} {
		if strings.Contains(out, s) {
			t.Errorf("unset role must be omitted, found %q", s)
		}
	}
}

func TestResolveRedisRoleManaged(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{JobQueue: &misskeyv1alpha1.RedisRole{}}
	p := resolve(m)
	ep, ok := p.redisRoles["jobQueue"]
	if !ok || !ep.managed || ep.host != "example-redis-jobqueue" {
		t.Errorf("managed jobQueue role wrong: %+v", ep)
	}
	if _, ok := p.redisRoles["pubsub"]; ok {
		t.Error("unset pubsub role must not appear")
	}
}

func TestResolveRedisRoleExternal(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{
		Pubsub: &misskeyv1alpha1.RedisRole{External: &misskeyv1alpha1.ExternalRedis{
			Host: "pubsub.redis.svc",
			PasswordSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "ps"}, Key: "pw",
			},
		}},
	}
	ep := resolve(m).redisRoles["pubsub"]
	if ep.managed || ep.host != "pubsub.redis.svc" || ep.passEnv != "REDIS_PASSWORD_PUBSUB" {
		t.Errorf("external pubsub role wrong: %+v", ep)
	}
}

func TestResolveRedisHADefault(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{}
	ep := resolve(m).redisDefault
	if !ep.managed || !ep.ha || ep.masterName != "mymaster" {
		t.Errorf("HA default endpoint wrong: %+v", ep)
	}
	if len(ep.sentinels) != 1 || ep.sentinels[0].host != "example-redis-sentinel" || ep.sentinels[0].port != 26379 {
		t.Errorf("HA sentinel endpoint wrong: %+v", ep.sentinels)
	}
}

func TestRenderRedisBlockSentinel(t *testing.T) {
	ep := redisEndpoint{host: "h", port: 6379, sentinels: []redisHostPort{{host: "s", port: 26379}}, masterName: "mymaster"}
	var b strings.Builder
	renderRedisBlock(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }, "redis", ep)
	out := b.String()
	for _, s := range []string{"sentinels:", "- host: s", "port: 26379", "name: mymaster"} {
		if !strings.Contains(out, s) {
			t.Errorf("sentinel block missing %q\n%s", s, out)
		}
	}
}

func TestRenderDefaultYMLRedisRolesAndHA(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{}
	// role単位で独立: jobQueueは自分のhaでHA、pubsubはha無しでstandalone
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{
		JobQueue: &misskeyv1alpha1.RedisRole{HA: &misskeyv1alpha1.RedisHA{}},
		Pubsub:   &misskeyv1alpha1.RedisRole{},
	}
	out := renderDefaultYML(m, resolve(m))
	// default redisとjobQueueはHA sentinel、pubsubはstandalone(sentinelなし)
	for _, s := range []string{"redis:", "sentinels:", "name: mymaster", "redisForJobQueue:", "host: example-redis-jobqueue-sentinel", "redisForPubsub:", "host: example-redis-pubsub"} {
		if !strings.Contains(out, s) {
			t.Errorf("expected %q in\n%s", s, out)
		}
	}
	if strings.Contains(out, "redisForTimelines") {
		t.Error("timelines role unset must be omitted")
	}
}

func TestManagedRedisInstances(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{
		JobQueue: &misskeyv1alpha1.RedisRole{},
		Pubsub:   &misskeyv1alpha1.RedisRole{External: &misskeyv1alpha1.ExternalRedis{Host: "x"}},
	}
	insts := managedRedisInstances(m)
	// default + jobQueue(managed)。pubsubはexternalで除外
	got := map[string]bool{}
	for _, i := range insts {
		got[i.suffix] = true
	}
	if !got[""] || !got["jobqueue"] || got["pubsub"] {
		t.Errorf("managed instances wrong: %+v", got)
	}
}

func redisInstanceBySuffix(m *misskeyv1alpha1.Misskey, suffix string) redisManagedInstance {
	for _, i := range managedRedisInstances(m) {
		if i.suffix == suffix {
			return i
		}
	}
	return redisManagedInstance{}
}

func TestBuildRedisReplicationAndSentinel(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{Replicas: 5, Sentinels: 3}
	inst := redisInstanceBySuffix(m, "")

	repl := buildRedisReplication(m, inst)
	if repl.GetKind() != "RedisReplication" || repl.GetName() != "example-redis" {
		t.Errorf("replication identity wrong: %s/%s", repl.GetKind(), repl.GetName())
	}
	rspec := repl.Object["spec"].(map[string]any)
	if rspec["clusterSize"] != int64(5) {
		t.Errorf("clusterSize: %v", rspec["clusterSize"])
	}
	if kc := rspec["kubernetesConfig"].(map[string]any); kc["image"] != "quay.io/opstree/redis:v8.2.5" {
		t.Errorf("image: %v", kc["image"])
	}
	if psc := rspec["podSecurityContext"].(map[string]any); psc["fsGroup"] != int64(1000) {
		t.Errorf("fsGroup must be 1000 (opstree non-root): %v", psc["fsGroup"])
	}
	rkc := rspec["kubernetesConfig"].(map[string]any)
	if rs, ok := rkc["redisSecret"].(map[string]any); !ok || rs["name"] != "example-redis-auth" {
		t.Errorf("HA replication must set redisSecret (requirepass): %v", rkc["redisSecret"])
	}
	if pvc, ok := rkc["persistentVolumeClaimRetentionPolicy"].(map[string]any); !ok || pvc["whenDeleted"] != "Retain" {
		t.Errorf("PVC retention must be Retain: %v", rkc["persistentVolumeClaimRetentionPolicy"])
	}

	sen := buildRedisSentinel(m, inst)
	sspec := sen.Object["spec"].(map[string]any)
	if sspec["clusterSize"] != int64(3) {
		t.Errorf("sentinel clusterSize: %v", sspec["clusterSize"])
	}
	rsc := sspec["redisSentinelConfig"].(map[string]any)
	if rsc["redisReplicationName"] != "example-redis" || rsc["masterGroupName"] != "mymaster" || rsc["quorum"] != "2" {
		t.Errorf("redisSentinelConfig wrong: %+v", rsc)
	}
}

func TestRedisEgressRule(t *testing.T) {
	m := newMisskey()
	if redisEgressRule(m) != nil {
		t.Error("no HA → redisEgressRule nil")
	}
	m.Spec.Redis.HA = &misskeyv1alpha1.RedisHA{}
	rr := redisEgressRule(m)
	if rr == nil || rr.To[0].PodSelector == nil {
		t.Fatalf("HA → egress rule expected: %+v", rr)
	}
	vals := rr.To[0].PodSelector.MatchExpressions[0].Values
	has := func(v string) bool {
		for _, x := range vals {
			if x == v {
				return true
			}
		}
		return false
	}
	if !has("example-redis") || !has("example-redis-sentinel") {
		t.Errorf("egress must select redis/sentinel operator pods (app=): %+v", vals)
	}
}

func TestDBEgressRule(t *testing.T) {
	// CNPGがpooler podのinstance labelを上書きするため、cnpg.io/clusterでdb/pooler宛egressを許可
	rule := dbEgressRule(newMisskey())
	if len(rule.To) != 1 || rule.To[0].PodSelector == nil {
		t.Fatalf("dbEgressRule should target a pod selector: %+v", rule)
	}
	if got := rule.To[0].PodSelector.MatchLabels["cnpg.io/cluster"]; got != "example-db" {
		t.Errorf("egress must select cnpg.io/cluster=example-db, got %q", got)
	}
}

func envHasName(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestMigrationConcurrentDefaultOff(t *testing.T) {
	// spec.migration未指定はopt-in既定off: CONCURRENTLYフラグを付けない
	m := newMisskey()
	job := buildMigrationJob(m, resolve(m))
	if envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("default (opt-in) must omit MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY")
	}
	// falseでも同様
	m.Spec.Migration.CreateIndexConcurrently = boolPtr(false)
	job = buildMigrationJob(m, resolve(m))
	if envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("createIndexConcurrently=false must omit the env")
	}
}

func TestMigrationConcurrentOptIn(t *testing.T) {
	m := newMisskey()
	m.Spec.Migration.CreateIndexConcurrently = boolPtr(true)
	job := buildMigrationJob(m, resolve(m))
	if !envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("createIndexConcurrently=true must set the env on the migration Job")
	}
	// app/workerには付かない(migration専用)
	for _, role := range []string{roleApp, roleWorker} {
		spec := buildMisskeyPodSpec(m, resolve(m), role, m.Spec.App)
		if envHasName(spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
			t.Errorf("%s must not carry the migration concurrency env", role)
		}
	}
}

func TestAutoscalingHelpers(t *testing.T) {
	if autoscalingEnabled(nil) {
		t.Error("nil autoscaling must be disabled")
	}
	// ポインタ存在=有効(内側Enabledは廃止)
	a := &misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 5}
	if !autoscalingEnabled(a) {
		t.Error("present block is enabled")
	}
	if autoscalingUsesKEDA(&misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 5}) {
		t.Error("no queues → native HPA")
	}
	if !autoscalingUsesKEDA(&misskeyv1alpha1.AutoscalingSpec{Queues: []misskeyv1alpha1.QueueScaleTrigger{{Name: "deliver", ListLength: 1}}}) {
		t.Error("queues → KEDA")
	}
}

func TestStaticReplicas(t *testing.T) {
	comp := misskeyv1alpha1.ComponentSpec{Replicas: int32Ptr(3)}
	if r := staticReplicas(comp); r == nil || *r != 3 {
		t.Errorf("no autoscaling → static replicas: %v", r)
	}
	comp.Autoscaling = &misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 5}
	if staticReplicas(comp) != nil {
		t.Error("autoscaling → replicas unmanaged (nil)")
	}
}

func TestBuildHPASpec(t *testing.T) {
	a := &misskeyv1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 10, TargetCPUUtilizationPercentage: int32Ptr(70)}
	spec := buildHPASpec("example-app", a)
	if spec.ScaleTargetRef.Name != "example-app" || spec.ScaleTargetRef.Kind != "Deployment" {
		t.Errorf("scaleTargetRef wrong: %+v", spec.ScaleTargetRef)
	}
	if spec.MinReplicas == nil || *spec.MinReplicas != 2 || spec.MaxReplicas != 10 {
		t.Errorf("min/max wrong: %v/%d", spec.MinReplicas, spec.MaxReplicas)
	}
	if len(spec.Metrics) != 1 || spec.Metrics[0].Resource.Name != corev1.ResourceCPU || *spec.Metrics[0].Resource.Target.AverageUtilization != 70 {
		t.Errorf("cpu metric wrong: %+v", spec.Metrics)
	}
	// 未指定はcpu80%にfallback(HPAは最低1 metric要)
	def := buildHPASpec("x", &misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 3})
	if len(def.Metrics) != 1 || *def.Metrics[0].Resource.Target.AverageUtilization != 80 {
		t.Errorf("default metric must be cpu 80: %+v", def.Metrics)
	}
	// memory
	mem := buildHPASpec("x", &misskeyv1alpha1.AutoscalingSpec{MaxReplicas: 3, TargetMemoryUtilizationPercentage: int32Ptr(75)})
	if len(mem.Metrics) != 1 || mem.Metrics[0].Resource.Name != corev1.ResourceMemory {
		t.Errorf("memory metric wrong: %+v", mem.Metrics)
	}
}

func TestJobQueueEndpoint(t *testing.T) {
	// role未分離 → default redis
	if ep := jobQueueEndpoint(resolve(newMisskey())); ep.host != "example-redis" {
		t.Errorf("default jobQueue endpoint: %q", ep.host)
	}
	// jobQueue分離 → 専用インスタンス
	m := newMisskey()
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{JobQueue: &misskeyv1alpha1.RedisRole{}}
	if ep := jobQueueEndpoint(resolve(m)); ep.host != "example-redis-jobqueue" {
		t.Errorf("separated jobQueue endpoint: %q", ep.host)
	}
}

func scaledObjectTriggers(so map[string]any) []any {
	return so["triggers"].([]any)
}

func TestBuildScaledObjectStandalone(t *testing.T) {
	m := newMisskey() // default redis standalone
	a := &misskeyv1alpha1.AutoscalingSpec{
		MinReplicas: int32Ptr(1), MaxReplicas: 30,
		Queues: []misskeyv1alpha1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000}, {Name: "inbox", ListLength: 500}},
	}
	so := buildScaledObject(m, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m)))
	if so.GetKind() != "ScaledObject" || so.GetName() != "example-worker" {
		t.Errorf("identity wrong: %s/%s", so.GetKind(), so.GetName())
	}
	spec := so.Object["spec"].(map[string]any)
	if spec["scaleTargetRef"].(map[string]any)["name"] != "example-worker" {
		t.Errorf("scaleTargetRef wrong: %v", spec["scaleTargetRef"])
	}
	if spec["maxReplicaCount"] != int64(30) || spec["minReplicaCount"] != int64(1) {
		t.Errorf("min/max wrong: %v/%v", spec["minReplicaCount"], spec["maxReplicaCount"])
	}
	trigs := scaledObjectTriggers(spec)
	if len(trigs) != 2 {
		t.Fatalf("expected 2 queue triggers, got %d", len(trigs))
	}
	t0 := trigs[0].(map[string]any)
	if t0["type"] != "redis" {
		t.Errorf("standalone must use redis trigger: %v", t0["type"])
	}
	meta := t0["metadata"].(map[string]any)
	if meta["address"] != "example-redis.ns.svc:6379" || meta["listLength"] != "1000" {
		t.Errorf("trigger meta wrong (FQDN address for cross-ns KEDA): %+v", meta)
	}
	if meta["listName"] != "misskey.example.com:queue:deliver:deliver:wait" {
		t.Errorf("computed listName wrong: %v", meta["listName"])
	}
}

func TestBuildScaledObjectSentinelAndOverride(t *testing.T) {
	// jobQueueをHA分離 → sentinel trigger、listName override
	m := newMisskey()
	m.Spec.Redis.Roles = &misskeyv1alpha1.RedisRoles{JobQueue: &misskeyv1alpha1.RedisRole{HA: &misskeyv1alpha1.RedisHA{}}}
	a := &misskeyv1alpha1.AutoscalingSpec{
		MaxReplicas: 20,
		Queues:      []misskeyv1alpha1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000, ListName: "custom:deliver:wait"}},
	}
	so := buildScaledObject(m, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m)))
	spec := so.Object["spec"].(map[string]any)
	if spec["minReplicaCount"] != int64(1) {
		t.Errorf("minReplicas unset must default to 1 (parity with HPA/godoc): %v", spec["minReplicaCount"])
	}
	trig := scaledObjectTriggers(spec)[0].(map[string]any)
	if trig["type"] != "redis-sentinel" {
		t.Errorf("HA jobQueue must use redis-sentinel trigger: %v", trig["type"])
	}
	meta := trig["metadata"].(map[string]any)
	if meta["hosts"] != "example-redis-jobqueue-sentinel.ns.svc" || meta["ports"] != "26379" || meta["sentinelMaster"] != "mymaster" {
		t.Errorf("sentinel meta wrong (hosts/ports): %+v", meta)
	}
	if meta["listName"] != "custom:deliver:wait" {
		t.Errorf("listName override ignored: %v", meta["listName"])
	}
}

func TestBuildScaledObjectExternalRedis(t *testing.T) {
	// external redisはcluster外hostのためFQDN(.ns.svc)化しない
	m := newMisskey()
	m.Spec.Redis.External = &misskeyv1alpha1.ExternalRedis{Host: "redis.prod.example.com", Port: 6380}
	a := &misskeyv1alpha1.AutoscalingSpec{
		MaxReplicas: 10,
		Queues:      []misskeyv1alpha1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000}},
	}
	so := buildScaledObject(m, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m)))
	trig := scaledObjectTriggers(so.Object["spec"].(map[string]any))[0].(map[string]any)
	meta := trig["metadata"].(map[string]any)
	if meta["address"] != "redis.prod.example.com:6380" {
		t.Errorf("external address must not be FQDN-ified: %v", meta["address"])
	}

	// external sentinelも同様にhostそのまま
	m2 := newMisskey()
	m2.Spec.Redis.External = &misskeyv1alpha1.ExternalRedis{
		Host: "redis.prod.example.com", MasterName: "mymaster",
		Sentinels: []misskeyv1alpha1.RedisHostPort{{Host: "s1.prod.example.com"}, {Host: "s2.prod.example.com", Port: 26380}},
	}
	so2 := buildScaledObject(m2, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m2)))
	trig2 := scaledObjectTriggers(so2.Object["spec"].(map[string]any))[0].(map[string]any)
	meta2 := trig2["metadata"].(map[string]any)
	if meta2["hosts"] != "s1.prod.example.com,s2.prod.example.com" || meta2["ports"] != "26379,26380" {
		t.Errorf("external sentinel hosts/ports must not be FQDN-ified: %+v", meta2)
	}
}

func TestMonitoringBuilders(t *testing.T) {
	m := newMisskey()
	m.Spec.Monitoring.Labels = map[string]string{"release": "kps"}

	sm := buildServiceMonitor(m, "meili-metrics", "meilisearch", selectorFor(m, "meilisearch"), "http", "/metrics", &metricsAuth{name: "sec", key: "MEILI_MASTER_KEY"})
	if sm.GetKind() != "ServiceMonitor" || sm.GetLabels()["release"] != "kps" {
		t.Errorf("SM kind/labels wrong: %s %v", sm.GetKind(), sm.GetLabels())
	}
	eps, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	ep0 := eps[0].(map[string]any)
	if ep0["port"] != "http" || ep0["path"] != "/metrics" {
		t.Errorf("SM endpoint: %v", ep0)
	}
	if _, ok := ep0["authorization"]; !ok {
		t.Error("meili SM must carry authorization")
	}

	pm := buildPodMonitor(m, "pg-metrics", "postgres", map[string]string{"cnpg.io/cluster": "x"}, "metrics", nil)
	pmeps, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	if pm.GetKind() != "PodMonitor" || pmeps[0].(map[string]any)["port"] != "metrics" {
		t.Errorf("PM wrong: %s %v", pm.GetKind(), pmeps)
	}

	c := redisExporterContainer(m)
	if c.Name != "metrics" || len(c.Ports) != 1 || c.Ports[0].ContainerPort != redisExporterPort {
		t.Errorf("exporter container wrong: %+v", c)
	}
}
