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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// resumeReplicas: suspend解除直後のautoscaling再点火を含むreplicas決定
func TestResumeReplicas(t *testing.T) {
	auto := func(min *int32) *scaleConfig {
		return workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
			AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MinReplicas: min, MaxReplicas: 5},
		})
	}
	existing := func(replicas int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(replicas)},
		}
	}
	cases := []struct {
		name string
		comp misskeyv1beta1.ComponentSpec
		sc   *scaleConfig
		dep  *appsv1.Deployment
		want *int32
	}{
		{"static", misskeyv1beta1.ComponentSpec{Replicas: int32Ptr(3)}, nil, existing(0), int32Ptr(3)},
		{"autoscaling+new", misskeyv1beta1.ComponentSpec{}, auto(nil), &appsv1.Deployment{}, nil},
		{"autoscaling+running", misskeyv1beta1.ComponentSpec{}, auto(nil), existing(2), nil},
		{"autoscaling+after suspend", misskeyv1beta1.ComponentSpec{}, auto(nil), existing(0), int32Ptr(1)},
		{"autoscaling+after suspend+minReplicas", misskeyv1beta1.ComponentSpec{}, auto(int32Ptr(2)), existing(0), int32Ptr(2)},
	}
	for _, tc := range cases {
		got := resumeReplicas(tc.comp, tc.sc, tc.dep)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: got %d, want nil", tc.name, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: got %v, want %d", tc.name, got, *tc.want)
		}
	}
}

func TestPoolerIgnoreStartupParameters(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{}
	u := buildPooler(m, nameDBPoolerRW(m), "rw")
	params, _, _ := unstructured.NestedStringMap(u.Object, "spec", "pgbouncer", "parameters")
	// transaction pooling下でMisskeyのstatement_timeoutを無視しないと接続失敗する回帰防止
	if !strings.Contains(params["ignore_startup_parameters"], "statement_timeout") {
		t.Errorf("pooler must ignore statement_timeout: %v", params)
	}
}

func newMisskey() *misskeyv1beta1.Misskey {
	return &misskeyv1beta1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "ns"},
		Spec: misskeyv1beta1.MisskeySpec{
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
	if !p.redisDefault.managed || p.redisDefault.host != "example-redis" {
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
	m.Spec.Postgres.External = &misskeyv1beta1.ExternalPostgres{
		Host: "pg.db.svc", Port: 6543, Database: "d", User: "u",
		PasswordSecret: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "pgsec"}, Key: "pw",
		},
	}
	m.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{Host: "redis.svc"}
	m.Spec.Search.Meilisearch.External = &misskeyv1beta1.ExternalMeilisearch{
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
	if p.redisDefault.managed || p.redisDefault.host != "redis.svc" {
		t.Errorf("external redis resolved wrong: %+v", p)
	}
	if p.meiliManaged || p.meiliHost != "meili.svc" || !p.meiliSSL {
		t.Errorf("external meili resolved wrong: %+v", p)
	}
	if p.meiliKeySel.Name != "meilisec" {
		t.Errorf("external meili key selector wrong: %+v", p.meiliKeySel)
	}
}

func objStorageSpec() *misskeyv1beta1.ObjectStorageSpec {
	return &misskeyv1beta1.ObjectStorageSpec{
		Bucket:   "media",
		Endpoint: "acct.r2.cloudflarestorage.com",
		Region:   "auto",
		BaseURL:  "https://cdn.example.com",
		Credentials: misskeyv1beta1.S3Credentials{
			AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "ak"},
			SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "sk"},
		},
	}
}

func TestResolveObjectStorage(t *testing.T) {
	// 未指定 → 無効・no-op
	if p := resolve(newMisskey()); p.objEnabled || p.objAutoConfigure {
		t.Errorf("unset objectStorage must be disabled: %+v", p)
	}

	// 指定 → 解決値とデフォルト
	m := newMisskey()
	m.Spec.ObjectStorage = objStorageSpec()
	p := resolve(m)
	if !p.objEnabled || !p.objAutoConfigure {
		t.Fatalf("objectStorage must be enabled and autoConfigure default true: %+v", p)
	}
	if p.objBucket != "media" || p.objEndpoint != "acct.r2.cloudflarestorage.com" || p.objBaseURL != "https://cdn.example.com" {
		t.Errorf("resolved values wrong: %+v", p)
	}
	if !p.objUseSSL || !p.objUseProxy || p.objSetPublicRead || !p.objForcePathStyle {
		t.Errorf("bool defaults wrong (useSSL/useProxy/!publicRead/forcePathStyle): %+v", p)
	}
	if p.objAccessKeySel.Name != "s3" || p.objAccessKeySel.Key != "ak" || p.objSecretKeySel.Key != "sk" {
		t.Errorf("credential selectors wrong: %+v", p)
	}
	if p.objImage != "ghcr.io/cloudnative-pg/postgresql:17" {
		t.Errorf("default image wrong: %q", p.objImage)
	}

	mr := newMisskey()
	os := objStorageSpec()
	os.Region = ""
	mr.Spec.ObjectStorage = os
	// リージョン未指定はus-east-1
	if pr := resolve(mr); pr.objRegion != "us-east-1" {
		t.Errorf("empty region must default to us-east-1, got %q", pr.objRegion)
	}
	// デフォルトカラム名
	if p.objColumns["bucket"] != "objectStorageBucket" || p.objColumns["secretKey"] != "objectStorageSecretKey" {
		t.Errorf("default column names wrong: %+v", p.objColumns)
	}

	// autoConfigure=false → 無効(operatorはmeta非管理)
	m2 := newMisskey()
	m2.Spec.ObjectStorage = objStorageSpec()
	m2.Spec.ObjectStorage.AutoConfigure = boolPtr(false)
	if p2 := resolve(m2); !p2.objEnabled || p2.objAutoConfigure {
		t.Errorf("autoConfigure=false must disable objAutoConfigure while enabled: %+v", p2)
	}

	// columnNames override
	m3 := newMisskey()
	m3.Spec.ObjectStorage = objStorageSpec()
	m3.Spec.ObjectStorage.ColumnNames = &misskeyv1beta1.ObjectStorageColumns{Bucket: "s3_bucket", SecretKey: "s3_secret"}
	p3 := resolve(m3)
	if p3.objColumns["bucket"] != "s3_bucket" || p3.objColumns["secretKey"] != "s3_secret" {
		t.Errorf("column override not applied: %+v", p3.objColumns)
	}
	if p3.objColumns["endpoint"] != "objectStorageEndpoint" {
		t.Errorf("non-overridden column must keep default: %+v", p3.objColumns)
	}
}

func TestRenderObjectStorageSQL(t *testing.T) {
	m := newMisskey()
	m.Spec.ObjectStorage = objStorageSpec()
	p := resolve(m)
	assigns, err := objectStorageAssignments(p)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	sql := renderObjectStorageSQL(assigns)

	// 構造
	for _, want := range []string{
		`\set ON_ERROR_STOP on`,
		`INSERT INTO meta (id) VALUES ('x') ON CONFLICT (id) DO NOTHING;`,
		`"useObjectStorage" = true`,
		`"objectStorageBucket" = :'v_bucket'`,
		`"objectStorageRegion" = :'v_region'`,
		`"objectStorageAccessKey" = :'v_accessKey'`,
		`"objectStorageSecretKey" = :'v_secretKey'`,
		`"objectStoragePort" = NULL`,   // port未指定
		`"objectStoragePrefix" = NULL`, // prefix未指定
		`"objectStorageSetPublicRead" = false`,
		`"objectStorageS3ForcePathStyle" = true`,
		`\getenv v_bucket OBJVAL_bucket`,
		`WHERE id = 'x';`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing %q:\n%s", want, sql)
		}
	}

	// 値・秘密がSQL本文に一切出ない(env経由のみ)
	for _, leak := range []string{"media", "cdn.example.com", "acct.r2.cloudflarestorage.com"} {
		if strings.Contains(sql, leak) {
			t.Errorf("SQL leaks value %q:\n%s", leak, sql)
		}
	}
}

func TestObjectStorageJobEnv(t *testing.T) {
	m := newMisskey()
	m.Spec.ObjectStorage = objStorageSpec()
	assigns, _ := objectStorageAssignments(resolve(m))
	env := objectStorageJobEnv(assigns)
	byName := map[string]corev1.EnvVar{}
	for _, e := range env {
		byName[e.Name] = e
	}
	// 平文値
	if byName["OBJVAL_bucket"].Value != "media" {
		t.Errorf("bucket env wrong: %+v", byName["OBJVAL_bucket"])
	}
	// 秘密はSecretKeyRef(平文Valueを持たない)
	ak := byName["OBJVAL_accessKey"]
	if ak.Value != "" || ak.ValueFrom == nil || ak.ValueFrom.SecretKeyRef == nil || ak.ValueFrom.SecretKeyRef.Key != "ak" {
		t.Errorf("accessKey must be SecretKeyRef: %+v", ak)
	}
	// 未設定optional(region未指定ならenv無し)ではなくregionはobjStorageSpecでauto設定済みなのでenvあり
	if byName["OBJVAL_region"].Value != "auto" {
		t.Errorf("region env wrong: %+v", byName["OBJVAL_region"])
	}
}

func TestObjectStorageColumnOverrideAndExtra(t *testing.T) {
	m := newMisskey()
	os := objStorageSpec()
	os.ColumnNames = &misskeyv1beta1.ObjectStorageColumns{Bucket: "s3_bucket"}
	os.ExtraColumns = map[string]string{"objectStorageSomethingNew": "v"}
	m.Spec.ObjectStorage = os
	assigns, err := objectStorageAssignments(resolve(m))
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	sql := renderObjectStorageSQL(assigns)
	if !strings.Contains(sql, `"s3_bucket" = :'v_bucket'`) {
		t.Errorf("column override not applied:\n%s", sql)
	}
	if !strings.Contains(sql, `"objectStorageSomethingNew" = :'x_0'`) {
		t.Errorf("extra column not applied:\n%s", sql)
	}
}

func TestObjectStorageIdentifierSafety(t *testing.T) {
	// 不正なoverrideカラム名はreject
	m := newMisskey()
	os := objStorageSpec()
	os.ColumnNames = &misskeyv1beta1.ObjectStorageColumns{Bucket: `bucket"; DROP TABLE meta; --`}
	m.Spec.ObjectStorage = os
	if _, err := objectStorageAssignments(resolve(m)); err == nil {
		t.Error("malicious column name must be rejected")
	}

	// extraキーが標準カラムと衝突→reject
	m2 := newMisskey()
	os2 := objStorageSpec()
	os2.ExtraColumns = map[string]string{"objectStorageBucket": "x"}
	m2.Spec.ObjectStorage = os2
	if _, err := objectStorageAssignments(resolve(m2)); err == nil {
		t.Error("extra column colliding with a standard column must be rejected")
	}
}

func TestResolveProviderSQLLike(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1beta1.SearchSQLLike
	p := resolve(m)
	if p.meiliEnabled {
		t.Errorf("sqlLike should not enable meilisearch")
	}
}

func TestResolveSetupPassword(t *testing.T) {
	// managed(生成)
	m := newMisskey()
	m.Spec.SetupPassword = &misskeyv1beta1.SetupPasswordSpec{}
	p := resolve(m)
	if !p.setupEnabled || !p.setupManaged || p.setupSel.Name != "example-setup" || p.setupSel.Key != setupPasswordID {
		t.Errorf("managed setup password wrong: %+v", p.setupSel)
	}

	// external secretRefの場合
	m2 := newMisskey()
	m2.Spec.SetupPassword = &misskeyv1beta1.SetupPasswordSpec{
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
	m.Spec.SetupPassword = &misskeyv1beta1.SetupPasswordSpec{}
	m.Spec.ExtraConfig = "maxFileSize: 100"
	out := renderDefaultYML(m, resolve(m))

	mustContain := []string{
		`url: "https://misskey.example.com/"`,
		`host: "example-db-rw"`,
		`user: "misskey"`,
		"pass: ${DB_PASSWORD}",
		`host: "example-redis"`,
		"provider: meilisearch",
		`host: "example-meilisearch"`,
		"apiKey: ${MEILI_KEY}",
		`index: "misskey-example-com"`,
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

func TestRenderRedisBlockExternalTLS(t *testing.T) {
	on := true
	m := newMisskey()
	m.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{Host: "redis.ext.svc", TLS: &on}
	out := renderDefaultYML(m, resolve(m))
	if !strings.Contains(out, "tls: {}") {
		t.Errorf("external redis TLS must emit tls: {}\n%s", out)
	}
	// TLS未指定は出力しない
	m2 := newMisskey()
	m2.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{Host: "redis.ext.svc"}
	if strings.Contains(renderDefaultYML(m2, resolve(m2)), "tls:") {
		t.Error("no TLS field must not emit tls:")
	}
}

func TestTruncateMsg(t *testing.T) {
	// 上限以内はそのまま
	if got := truncateMsg("short"); got != "short" {
		t.Errorf("short passthrough: %q", got)
	}
	// 接尾辞込みで1024バイト以内
	long := strings.Repeat("a", 5000)
	got := truncateMsg(long)
	if len(got) > 1024 {
		t.Errorf("truncated length %d exceeds 1024", len(got))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("missing truncation suffix: ...%q", got[len(got)-20:])
	}
}

func TestRenderDefaultYMLSQLLike(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1beta1.SearchSQLPgroonga
	out := renderDefaultYML(m, resolve(m))
	if !strings.Contains(out, "provider: sqlPgroonga") {
		t.Errorf("expected sqlPgroonga provider")
	}
	if strings.Contains(out, "meilisearch:") || strings.Contains(out, "MEILI_KEY") {
		t.Errorf("non-meili provider must not emit a meilisearch block:\n%s", out)
	}
}

func TestRenderDefaultYMLPerformanceProxyFiles(t *testing.T) {
	m := newMisskey()
	m.Spec.Performance = misskeyv1beta1.PerformanceSpec{
		DeliverJobConcurrency: int32Ptr(64),
		InboxJobConcurrency:   int32Ptr(32),
		DeliverJobPerSec:      int32Ptr(128),
		InboxJobPerSec:        int32Ptr(64),
		RelationshipJobPerSec: int32Ptr(16),
		DeliverJobMaxAttempts: int32Ptr(10),
		InboxJobMaxAttempts:   int32Ptr(8),
	}
	m.Spec.OutboundProxy = misskeyv1beta1.OutboundProxySpec{
		HTTP: "http://proxy:3128", SMTP: "http://proxy:3128",
		BypassHosts: []string{"hcaptcha.com", "challenges.cloudflare.com"},
	}
	m.Spec.Files = misskeyv1beta1.FilesSpec{
		MaxFileSize:      int64Ptr(262144000),
		MediaProxy:       "https://mp.example.com/proxy",
		ProxyRemoteFiles: boolPtr(false),
	}
	out := renderDefaultYML(m, resolve(m))
	for _, s := range []string{
		"deliverJobConcurrency: 64",
		"inboxJobConcurrency: 32",
		"deliverJobPerSec: 128",
		"inboxJobPerSec: 64",
		"relationshipJobPerSec: 16",
		"deliverJobMaxAttempts: 10",
		"inboxJobMaxAttempts: 8",
		`proxy: "http://proxy:3128"`,
		`proxySmtp: "http://proxy:3128"`,
		`proxyBypassHosts: ["hcaptcha.com", "challenges.cloudflare.com"]`,
		"maxFileSize: 262144000",
		`mediaProxy: "https://mp.example.com/proxy"`,
		"proxyRemoteFiles: false",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("default.yml missing %q\n---\n%s", s, out)
		}
	}
}

func TestRenderDefaultYMLTuningOmittedByDefault(t *testing.T) {
	out := renderDefaultYML(newMisskey(), resolve(newMisskey()))
	// 既定はproxyRemoteFiles: trueのみ出力。他のknobsは出さず既存出力を不変に保つ
	if !strings.Contains(out, "proxyRemoteFiles: true") {
		t.Errorf("proxyRemoteFiles: true must be emitted by default:\n%s", out)
	}
	for _, s := range []string{
		"deliverJobConcurrency", "inboxJobConcurrency", "deliverJobPerSec",
		"relationshipJobPerSec", "JobMaxAttempts", "proxy:", "proxySmtp",
		"proxyBypassHosts", "maxFileSize", "mediaProxy",
	} {
		if strings.Contains(out, s) {
			t.Errorf("unset tuning key %q must be omitted:\n%s", s, out)
		}
	}
}

func TestRenderCaddyfileDefaults(t *testing.T) {
	out := renderCaddyfile(newMisskey())

	mustContain := []string{
		"reverse_proxy example-app:3000",
		"health_uri /api/server-info",
		"@api path /api/*",
		`respond "" {err.status_code}`,
		"root * /usr/share/caddy", // メンテページはproxy自身のfile_serverで配信
		"rewrite * /maintenance.html",
		"status 200", // メンテナンスの既定ステータス
		`header Cache-Control "no-store"`,
		"metrics", // per-handlerメトリクス有効化
		":9180 {", // metrics専用リスナ
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("Caddyfile missing %q\n---\n%s", s, out)
		}
	}
	// 統合後はmaintenance Serviceへのreverse_proxyを持たない
	if strings.Contains(out, "example-maintenance") {
		t.Errorf("Caddyfile must not proxy to the maintenance service:\n%s", out)
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
	if !strings.Contains(out, "status 503") {
		t.Errorf("expected configurable maintenance status 503:\n%s", out)
	}
}

func TestRenderCaddyfileMaintenanceDisabled(t *testing.T) {
	m := newMisskey()
	m.Spec.Proxy.Maintenance.Enabled = boolPtr(false)
	out := renderCaddyfile(m)

	if strings.Contains(out, "handle_errors") {
		t.Errorf("maintenance disabled must omit handle_errors:\n%s", out)
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
	// 値はJSON.stringifyでquoteし、改行・#等を含む値でもYAMLとして安全に埋め込む
	if !strings.Contains(renderConfigScript, "JSON.stringify(v)") {
		t.Errorf("render script must JSON-quote values for YAML safety")
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
		t.Error("tenant leaked into selectorFor")
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
	m.Spec.Runtime = misskeyv1beta1.RuntimeSpec{
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
	spec := buildMisskeyPodSpec(m, p, roleApp, m.Spec.App.ComponentSpec)
	if spec.SecurityContext.RunAsUser == nil || *spec.SecurityContext.RunAsUser != 991 {
		t.Error("default uid != 991")
	}
	c := spec.Containers[0]
	if strings.Join(c.Command, " ") != "pnpm run start" {
		t.Errorf("default command: %v", c.Command)
	}
	if !hasMount(c.VolumeMounts, "/misskey/.config/default.yml") {
		t.Error("config mount missing")
	}
	if !hasMount(c.VolumeMounts, "/misskey/built") {
		t.Error("built mount missing")
	}
	if !hasContainer(spec.InitContainers, "prepare-built") {
		t.Error("prepare-built init missing")
	}

	// builtPath="" → built mount/prepare-built無し
	empty := ""
	m.Spec.Runtime.BuiltPath = &empty
	spec = buildMisskeyPodSpec(m, p, roleApp, m.Spec.App.ComponentSpec)
	if hasMount(spec.Containers[0].VolumeMounts, "/misskey/built") {
		t.Error("built mount remains with empty builtPath")
	}
	if hasContainer(spec.InitContainers, "prepare-built") {
		t.Error("prepare-built remains with empty builtPath")
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
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{}
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
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{}
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
	for _, s := range []string{"dbReplications: true", "dbSlaves:", `host: "example-db-ro"`, "pass: ${DB_PASSWORD}"} {
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
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{}
	mp := migratePlan(m, resolve(m))
	if mp.dbHost != "example-db-rw" {
		t.Errorf("migration must bypass pooler to -rw: %q", mp.dbHost)
	}
	if mp.dbReplications || len(mp.dbSlaves) != 0 {
		t.Errorf("migration must not use replicas: %+v", mp)
	}
	out := renderDefaultYML(m, mp)
	if !strings.Contains(out, `host: "example-db-rw"`) || !strings.Contains(out, "dbReplications: false") {
		t.Errorf("migrate config not primary-direct:\n%s", out)
	}
}

func TestBuildPooler(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{
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

// recovery付きfixture
func withRecovery(m *misskeyv1beta1.Misskey) *misskeyv1beta1.Misskey {
	m.Spec.Postgres.Recovery = &misskeyv1beta1.PostgresRecovery{
		Source: misskeyv1beta1.RecoverySource{
			DestinationPath: "s3://bk/misskey",
			EndpointURL:     "https://s3.example.com",
			ServerName:      "old-db",
			S3Credentials: &misskeyv1beta1.S3Credentials{
				AccessKeyID:     corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "id"},
				SecretAccessKey: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}, Key: "secret"},
			},
		},
	}
	return m
}

func TestBuildDBClusterInitdbDefaults(t *testing.T) {
	cluster := buildDBCluster(newMisskey())

	if cluster.GetName() != "example-db" || cluster.GetKind() != "Cluster" {
		t.Errorf("cluster identity wrong: %s/%s", cluster.GetKind(), cluster.GetName())
	}
	spec := cluster.Object["spec"].(map[string]any)
	initdb := spec["bootstrap"].(map[string]any)["initdb"].(map[string]any)
	if initdb["database"] != "misskey" || initdb["owner"] != "misskey" {
		t.Errorf("initdb defaults: %+v", initdb)
	}
	if _, ok := initdb["postInitApplicationSQL"]; ok {
		t.Errorf("postInitApplicationSQL must be absent without pgroonga: %+v", initdb)
	}
	if _, ok := spec["externalClusters"]; ok {
		t.Error("externalClusters must be absent without recovery")
	}
	if spec["storage"].(map[string]any)["size"] != "20Gi" {
		t.Errorf("storage default: %+v", spec["storage"])
	}
	labels := spec["inheritedMetadata"].(map[string]any)["labels"].(map[string]any)
	if labels["app.kubernetes.io/instance"] != "example" {
		t.Errorf("inherited labels: %+v", labels)
	}
}

func TestBuildDBClusterPgroongaInitdb(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1beta1.SearchSQLPgroonga
	spec := buildDBCluster(m).Object["spec"].(map[string]any)
	initdb := spec["bootstrap"].(map[string]any)["initdb"].(map[string]any)
	sqls, ok := initdb["postInitApplicationSQL"].([]any)
	if !ok || len(sqls) != 1 || sqls[0] != "CREATE EXTENSION IF NOT EXISTS pgroonga" {
		t.Errorf("postInitApplicationSQL: %+v", initdb)
	}
}

func TestBuildDBClusterRecovery(t *testing.T) {
	m := withRecovery(newMisskey())
	spec := buildDBCluster(m).Object["spec"].(map[string]any)

	bootstrap := spec["bootstrap"].(map[string]any)
	if _, ok := bootstrap["initdb"]; ok {
		t.Errorf("initdb must be absent with recovery: %+v", bootstrap)
	}
	rec := bootstrap["recovery"].(map[string]any)
	if rec["source"] != "origin" || rec["database"] != "misskey" || rec["owner"] != "misskey" {
		t.Errorf("recovery bootstrap: %+v", rec)
	}
	if _, ok := rec["recoveryTarget"]; ok {
		t.Errorf("recoveryTarget must be absent without targetTime: %+v", rec)
	}
	ec := spec["externalClusters"].([]any)[0].(map[string]any)
	if ec["name"] != "origin" {
		t.Errorf("externalCluster name: %+v", ec)
	}
	barman := ec["barmanObjectStore"].(map[string]any)
	if barman["destinationPath"] != "s3://bk/misskey" || barman["serverName"] != "old-db" || barman["endpointURL"] != "https://s3.example.com" {
		t.Errorf("barmanObjectStore: %+v", barman)
	}
	if barman["wal"].(map[string]any)["maxParallel"] != int64(8) {
		t.Errorf("wal.maxParallel: %+v", barman["wal"])
	}
	if barman["s3Credentials"].(map[string]any)["accessKeyId"].(map[string]any)["name"] != "s3" {
		t.Errorf("s3Credentials: %+v", barman["s3Credentials"])
	}
}

func TestBuildDBClusterRecoveryPgroonga(t *testing.T) {
	m := withRecovery(newMisskey())
	m.Spec.Search.Provider = misskeyv1beta1.SearchSQLPgroonga
	spec := buildDBCluster(m).Object["spec"].(map[string]any)
	// recovery時は出力しない(拡張は復元データに含まれる)
	if strings.Contains(fmt.Sprintf("%v", spec), "postInitApplicationSQL") {
		t.Errorf("postInitApplicationSQL must be absent with recovery:\n%v", spec)
	}
}

func TestBuildDBClusterRecoveryTargetTime(t *testing.T) {
	m := withRecovery(newMisskey())
	m.Spec.Postgres.Recovery.TargetTime = "2026-07-15T00:00:00+09:00"
	spec := buildDBCluster(m).Object["spec"].(map[string]any)
	rt := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"].(map[string]any)
	if rt["targetTime"] != "2026-07-15T00:00:00+09:00" {
		t.Errorf("targetTime: %+v", rt)
	}
}

func TestBuildDBClusterBackupServerName(t *testing.T) {
	m := withRecovery(newMisskey())
	m.Spec.Postgres.Backup = &misskeyv1beta1.PostgresBackup{
		DestinationPath: "s3://bk/misskey",
		ServerName:      "example-db-restored",
	}
	spec := buildDBCluster(m).Object["spec"].(map[string]any)
	barman := spec["backup"].(map[string]any)["barmanObjectStore"].(map[string]any)
	if barman["serverName"] != "example-db-restored" {
		t.Errorf("backup serverName: %+v", barman)
	}
	// recoveryとbackupは併存し双方描画される
	if _, ok := spec["externalClusters"]; !ok {
		t.Error("externalClusters missing alongside backup")
	}

	m.Spec.Postgres.Backup.ServerName = ""
	spec = buildDBCluster(m).Object["spec"].(map[string]any)
	barman = spec["backup"].(map[string]any)["barmanObjectStore"].(map[string]any)
	if _, ok := barman["serverName"]; ok {
		t.Errorf("empty serverName must be omitted: %+v", barman)
	}
}

func TestBuildDBClusterImport(t *testing.T) {
	m := newMisskey()
	m.Spec.Search.Provider = misskeyv1beta1.SearchSQLPgroonga
	m.Spec.Postgres.Import = &misskeyv1beta1.PostgresImport{Source: misskeyv1beta1.ImportSource{
		Host: "old-pg", Database: "misskey0", User: "mk",
		PasswordSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "srcpw"}, Key: "pw"},
	}}
	spec := buildDBCluster(m).Object["spec"].(map[string]any)

	bootstrap := spec["bootstrap"].(map[string]any)
	if _, ok := bootstrap["recovery"]; ok {
		t.Errorf("recovery must be absent with import: %+v", bootstrap)
	}
	initdb := bootstrap["initdb"].(map[string]any)
	if initdb["database"] != "misskey" || initdb["owner"] != "misskey" {
		t.Errorf("initdb defaults: %+v", initdb)
	}
	// pgroongaのpostInitはimport時も維持(restore前に拡張を用意)
	if _, ok := initdb["postInitApplicationSQL"]; !ok {
		t.Errorf("postInitApplicationSQL missing: %+v", initdb)
	}
	imp := initdb["import"].(map[string]any)
	if imp["type"] != "microservice" {
		t.Errorf("import type: %+v", imp)
	}
	if dbs := imp["databases"].([]any); len(dbs) != 1 || dbs[0] != "misskey0" {
		t.Errorf("import databases: %+v", imp)
	}
	if imp["source"].(map[string]any)["externalCluster"] != "origin" {
		t.Errorf("import source: %+v", imp)
	}
	ec := spec["externalClusters"].([]any)[0].(map[string]any)
	conn := ec["connectionParameters"].(map[string]any)
	if conn["host"] != "old-pg" || conn["port"] != "5432" || conn["user"] != "mk" || conn["dbname"] != "misskey0" || conn["sslmode"] != "prefer" {
		t.Errorf("connectionParameters: %+v", conn)
	}
	if ec["password"].(map[string]any)["name"] != "srcpw" {
		t.Errorf("password ref: %+v", ec["password"])
	}
}

func TestBuildPreMigrationBackup(t *testing.T) {
	m := newMisskey()
	b := buildPreMigrationBackup(m)
	if b.GetKind() != "Backup" || b.GetNamespace() != "ns" {
		t.Errorf("backup identity wrong: %s/%s", b.GetKind(), b.GetName())
	}
	if b.GetName() != namePreBackup(m) || !strings.HasPrefix(b.GetName(), "example-premigrate-") {
		t.Errorf("backup name: %s", b.GetName())
	}
	spec := b.Object["spec"].(map[string]any)
	if spec["cluster"].(map[string]any)["name"] != "example-db" {
		t.Errorf("cluster ref: %+v", spec)
	}
	if b.GetLabels()["app.kubernetes.io/component"] != "premigrate" {
		t.Errorf("labels: %+v", b.GetLabels())
	}
	// image変更で別名(=別Backup)になる
	m2 := newMisskey()
	m2.Spec.Image = "misskey/misskey:other"
	if namePreBackup(m2) == namePreBackup(m) {
		t.Error("pre-backup name unchanged after image change")
	}
}

func TestPreMigrationBackupGate(t *testing.T) {
	r := &MisskeyReconciler{}
	ctx := t.Context()
	// 無効/バックアップ未設定/external DBはno-opでgate通過(clientに触れない)
	cases := []struct {
		name string
		m    *misskeyv1beta1.Misskey
		p    plan
	}{
		{"preBackup disabled", newMisskey(), plan{dbManaged: true}},
		{"backup unset", func() *misskeyv1beta1.Misskey {
			m := newMisskey()
			m.Spec.Migration.PreBackup = boolPtr(true)
			return m
		}(), plan{dbManaged: true}},
		{"external DB", func() *misskeyv1beta1.Misskey {
			m := newMisskey()
			m.Spec.Migration.PreBackup = boolPtr(true)
			m.Spec.Postgres.Backup = &misskeyv1beta1.PostgresBackup{DestinationPath: "s3://b"}
			return m
		}(), plan{dbManaged: false}},
	}
	for _, tc := range cases {
		ok, err := r.reconcilePreMigrationBackup(ctx, tc.m, tc.p)
		if !ok || err != nil {
			t.Errorf("%s: gate must pass as no-op, got ok=%v err=%v", tc.name, ok, err)
		}
	}
}

func TestBuildDBVerifyCluster(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Backup = &misskeyv1beta1.PostgresBackup{
		DestinationPath: "s3://bk/misskey",
		EndpointURL:     "https://s3.example.com",
		Verify:          &misskeyv1beta1.BackupVerify{},
	}
	vc := buildDBVerifyCluster(m)

	if vc.GetName() != "example-db-verify" || vc.GetKind() != "Cluster" {
		t.Errorf("verify cluster identity wrong: %s/%s", vc.GetKind(), vc.GetName())
	}
	spec := vc.Object["spec"].(map[string]any)
	if spec["instances"] != int64(1) {
		t.Errorf("instances: %v", spec["instances"])
	}
	// 自前バックアップからのrecovery bootstrap、backupセクションなし(WALアーカイブ衝突防止)
	rec := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)
	if rec["source"] != "origin" {
		t.Errorf("recovery source: %+v", rec)
	}
	if _, ok := spec["backup"]; ok {
		t.Error("verify cluster must not archive WALs")
	}
	barman := spec["externalClusters"].([]any)[0].(map[string]any)["barmanObjectStore"].(map[string]any)
	// serverName未指定時は本体クラスタ名のフォルダから復元
	if barman["serverName"] != "example-db" || barman["destinationPath"] != "s3://bk/misskey" {
		t.Errorf("barmanObjectStore: %+v", barman)
	}
	if vc.GetLabels()["app.kubernetes.io/component"] != "postgres-verify" {
		t.Errorf("labels: %+v", vc.GetLabels())
	}

	// backup.serverName指定時はそれを復元元にする
	m.Spec.Postgres.Backup.ServerName = "renamed"
	barman = buildDBVerifyCluster(m).Object["spec"].(map[string]any)["externalClusters"].([]any)[0].(map[string]any)["barmanObjectStore"].(map[string]any)
	if barman["serverName"] != "renamed" {
		t.Errorf("serverName override: %+v", barman)
	}
}

func TestBackupVerifyDue(t *testing.T) {
	now := metav1.Now()
	m := newMisskey()
	if !backupVerifyDue(m, time.Hour, now.Time) {
		t.Error("first run should be immediately due")
	}
	m.Status.BackupVerification = &misskeyv1beta1.BackupVerificationStatus{LastVerifiedTime: now}
	if backupVerifyDue(m, time.Hour, now.Add(30*time.Minute)) {
		t.Error("due before interval elapsed")
	}
	if !backupVerifyDue(m, time.Hour, now.Add(2*time.Hour)) {
		t.Error("not due after interval elapsed")
	}
}

func TestIngressAnnotationsIssuerRef(t *testing.T) {
	m := newMisskey()
	// 既定: nginx body-sizeのみ
	ann := ingressAnnotations(m, "nginx")
	if ann["nginx.ingress.kubernetes.io/proxy-body-size"] != "0" || len(ann) != 1 {
		t.Errorf("default annotations: %+v", ann)
	}
	// ClusterIssuer(既定kind)
	m.Spec.Ingress.IssuerRef = &misskeyv1beta1.IngressIssuerRef{Name: "letsencrypt"}
	ann = ingressAnnotations(m, "nginx")
	if ann["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("cluster-issuer annotation: %+v", ann)
	}
	// namespaced Issuer
	m.Spec.Ingress.IssuerRef.Kind = "Issuer"
	ann = ingressAnnotations(m, "nginx")
	if ann["cert-manager.io/issuer"] != "letsencrypt" {
		t.Errorf("issuer annotation: %+v", ann)
	}
	if _, ok := ann["cert-manager.io/cluster-issuer"]; ok {
		t.Errorf("both issuer annotations set: %+v", ann)
	}
	// operator管理キー(cert-manager)はユーザ指定で上書きできない(後勝ち)
	m.Spec.Ingress.Annotations = map[string]string{"cert-manager.io/issuer": "custom"}
	if ann = ingressAnnotations(m, "nginx"); ann["cert-manager.io/issuer"] != "letsencrypt" {
		t.Errorf("operator cert-manager annotation must win over user override: %+v", ann)
	}
}

func TestIngressAnnotationsDenylist(t *testing.T) {
	m := newMisskey()
	m.Spec.Ingress.Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/configuration-snippet": "return 200 pwned;",
		"nginx.ingress.kubernetes.io/server-snippet":        "location / {}",
		"nginx.ingress.kubernetes.io/auth-url":              "http://evil/auth",
		"nginx.ingress.kubernetes.io/auth-tls-secret":       "ns/secret",
		"nginx.ingress.kubernetes.io/proxy-connect-timeout": "5", // 良性は残す
	}
	ann := ingressAnnotations(m, "nginx")
	for _, k := range []string{
		"nginx.ingress.kubernetes.io/configuration-snippet",
		"nginx.ingress.kubernetes.io/server-snippet",
		"nginx.ingress.kubernetes.io/auth-url",
		"nginx.ingress.kubernetes.io/auth-tls-secret",
	} {
		if _, ok := ann[k]; ok {
			t.Errorf("dangerous annotation %q must be dropped: %+v", k, ann)
		}
	}
	if ann["nginx.ingress.kubernetes.io/proxy-connect-timeout"] != "5" {
		t.Errorf("benign annotation must pass through: %+v", ann)
	}
}

func TestDigestResolverPinned(t *testing.T) {
	ctx := t.Context()
	calls := 0
	dr := NewDigestResolver()
	dr.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		calls++
		return "sha256:aaa", nil
	}

	// digest指定済みはレジストリに触れずそのまま
	if got, err := dr.Pinned(ctx, "img:v1@sha256:zzz", nil, ""); err != nil || got != "img:v1@sha256:zzz" || calls != 0 {
		t.Errorf("pre-pinned passthrough: %v %v calls=%d", got, err, calls)
	}
	// 解決+TTL内cache
	if got, _ := dr.Pinned(ctx, "img:latest", nil, ""); got != "img:latest@sha256:aaa" {
		t.Errorf("pinned: %v", got)
	}
	if _, _ = dr.Pinned(ctx, "img:latest", nil, ""); calls != 1 {
		t.Errorf("cache should be used within TTL: calls=%d", calls)
	}
	// TTL切れで再解決
	dr.cache[cacheKey("img:latest", "")] = digestEntry{digest: "sha256:aaa", resolvedAt: time.Now().Add(-time.Hour)}
	dr.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		return "sha256:bbb", nil
	}
	if got, _ := dr.Pinned(ctx, "img:latest", nil, ""); got != "img:latest@sha256:bbb" {
		t.Errorf("re-resolve after TTL expiry: %v", got)
	}
	// 失敗時はstale cacheへfallback
	dr.cache[cacheKey("img:latest", "")] = digestEntry{digest: "sha256:bbb", resolvedAt: time.Now().Add(-time.Hour)}
	dr.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		return "", fmt.Errorf("registry down")
	}
	if got, err := dr.Pinned(ctx, "img:latest", nil, ""); err != nil || got != "img:latest@sha256:bbb" {
		t.Errorf("stale fallback: %v %v", got, err)
	}
	// cache無しの失敗はエラー
	if _, err := dr.Pinned(ctx, "other:latest", nil, ""); err == nil {
		t.Error("failure without cache should be an error")
	}
}

// keyIDが異なればcacheを共有せず、テナント跨ぎでprivate digestが漏れない
func TestDigestResolverKeyIsolation(t *testing.T) {
	ctx := t.Context()
	got := map[string]int{}
	dr := NewDigestResolver()
	dr.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		got["calls"]++
		return "sha256:aaa", nil
	}
	// テナントA(keyID="ns-a")で解決
	if _, err := dr.Pinned(ctx, "priv:latest", nil, "ns-a"); err != nil {
		t.Fatalf("tenant A resolve: %v", err)
	}
	// テナントB(keyID="ns-b")は別keyのため再解決される(A のcacheヒットで即返さない)
	if _, err := dr.Pinned(ctx, "priv:latest", nil, "ns-b"); err != nil {
		t.Fatalf("tenant B resolve: %v", err)
	}
	if got["calls"] != 2 {
		t.Errorf("different keyID must not share cache: calls=%d", got["calls"])
	}
}

func TestResolveImageTrackDigest(t *testing.T) {
	ctx := t.Context()
	dr := NewDigestResolver()
	dr.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		return "sha256:abc", nil
	}
	r := &MisskeyReconciler{Digests: dr}

	m := newMisskey()
	m.Spec.Image = "misskey/misskey:latest"
	m.Spec.TrackImageDigest = true
	if err := r.resolveImage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Image != "misskey/misskey:latest@sha256:abc" {
		t.Errorf("pinned image: %s", m.Spec.Image)
	}

	// 解決失敗+cache無しでも直前のstatus pinがあれば継続(flap防止)
	dr2 := NewDigestResolver()
	dr2.headFunc = func(_ context.Context, _ string, _ authn.Keychain) (string, error) {
		return "", fmt.Errorf("registry down")
	}
	r2 := &MisskeyReconciler{Digests: dr2}
	m2 := newMisskey()
	m2.Spec.Image = "misskey/misskey:latest"
	m2.Spec.TrackImageDigest = true
	m2.Status.Image = "misskey/misskey:latest@sha256:old"
	if err := r2.resolveImage(ctx, m2); err != nil {
		t.Fatal(err)
	}
	if m2.Spec.Image != "misskey/misskey:latest@sha256:old" {
		t.Errorf("status pin should persist: %s", m2.Spec.Image)
	}

	// statusにも無い初回失敗はエラー
	m3 := newMisskey()
	m3.Spec.Image = "misskey/misskey:latest"
	m3.Spec.TrackImageDigest = true
	if err := r2.resolveImage(ctx, m3); err == nil {
		t.Error("first-time resolution failure should be an error")
	}

	// 追従off/digest指定済みは無変換
	m4 := newMisskey()
	m4.Spec.Image = "misskey/misskey:2026.6.0"
	if err := r2.resolveImage(ctx, m4); err != nil || m4.Spec.Image != "misskey/misskey:2026.6.0" {
		t.Errorf("tracking off should leave image unchanged: %s %v", m4.Spec.Image, err)
	}
}

func TestChannelBucket(t *testing.T) {
	// 決定性
	b1, b2 := channelBucket("ns", "a"), channelBucket("ns", "a")
	if b1 != b2 {
		t.Error("bucket must be deterministic")
	}
	// 0-99域と分散(全一致しないこと)
	seen := map[uint32]bool{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		b := channelBucket("ns", n)
		if b > 99 {
			t.Errorf("bucket out of range: %d", b)
		}
		seen[b] = true
	}
	if len(seen) < 2 {
		t.Errorf("buckets not distributed: %v", seen)
	}
}

func TestChannelImageFor(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	ch := func(mutate func(*misskeyv1beta1.MisskeyChannel)) *misskeyv1beta1.MisskeyChannel {
		c := &misskeyv1beta1.MisskeyChannel{
			Spec: misskeyv1beta1.MisskeyChannelSpec{
				Image:   "v2",
				Rollout: &misskeyv1beta1.ChannelRollout{BatchPercent: 20, Interval: metav1.Duration{Duration: time.Hour}},
			},
			Status: misskeyv1beta1.MisskeyChannelStatus{
				Image: "v2", PreviousImage: "v1", ImageChangedAt: metav1.NewTime(now),
			},
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}

	// status未反映のbootstrapはspec直
	c := ch(func(c *misskeyv1beta1.MisskeyChannel) { c.Status = misskeyv1beta1.MisskeyChannelStatus{} })
	if got := channelImageFor(c, 99, now); got != "v2" {
		t.Errorf("bootstrap: %s", got)
	}
	// rollout未設定は即時全量
	c = ch(func(c *misskeyv1beta1.MisskeyChannel) { c.Spec.Rollout = nil })
	if got := channelImageFor(c, 99, now); got != "v2" {
		t.Errorf("no rollout: %s", got)
	}
	// previous無しは即時全量
	c = ch(func(c *misskeyv1beta1.MisskeyChannel) { c.Status.PreviousImage = "" })
	if got := channelImageFor(c, 99, now); got != "v2" {
		t.Errorf("no previous: %s", got)
	}
	// t=0: 第1バッチ(bucket<20)のみv2
	c = ch(nil)
	if got := channelImageFor(c, 19, now); got != "v2" {
		t.Errorf("t=0 bucket19: %s", got)
	}
	if got := channelImageFor(c, 20, now); got != "v1" {
		t.Errorf("t=0 bucket20: %s", got)
	}
	// t=1h: 閾値40
	if got := channelImageFor(c, 39, now.Add(time.Hour)); got != "v2" {
		t.Errorf("t=1h bucket39: %s", got)
	}
	if got := channelImageFor(c, 40, now.Add(time.Hour)); got != "v1" {
		t.Errorf("t=1h bucket40: %s", got)
	}
	// t=4h: 全量
	if got := channelImageFor(c, 99, now.Add(4*time.Hour)); got != "v2" {
		t.Errorf("t=4h bucket99: %s", got)
	}
}

func TestBuildScaledObjectRPS(t *testing.T) {
	m := newMisskey()
	a := appScaleConfig(&misskeyv1beta1.AppAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 5},
		RPS:             &misskeyv1beta1.RPSTrigger{ServerAddress: "http://prom:9090", TargetRPS: 50},
	})
	if !autoscalingUsesKEDA(a) {
		t.Fatal("rps setting should take the KEDA path")
	}
	so := buildScaledObject(m, roleApp, nameApp(m), a, redisEndpoint{})
	triggers := so.Object["spec"].(map[string]any)["triggers"].([]any)
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d: %+v", len(triggers), triggers)
	}
	trig := triggers[0].(map[string]any)
	if trig["type"] != "prometheus" {
		t.Errorf("trigger type: %v", trig["type"])
	}
	meta := trig["metadata"].(map[string]any)
	if meta["serverAddress"] != "http://prom:9090" || meta["threshold"] != "50" {
		t.Errorf("trigger metadata: %+v", meta)
	}
	// 既定queryは自インスタンスのproxyに限定
	if q := meta["query"].(string); !strings.Contains(q, `service="example-proxy"`) || !strings.Contains(q, `namespace="ns"`) {
		t.Errorf("default query: %s", q)
	}
	if _, ok := trig["authenticationRef"]; ok {
		t.Error("redis authenticationRef attached to rps trigger")
	}

	// query上書き + cpu floor併存
	a.rps.Query = "sum(custom_metric)"
	a.TargetCPUUtilizationPercentage = int32Ptr(70)
	triggers = buildScaledObject(m, roleApp, nameApp(m), a, redisEndpoint{}).Object["spec"].(map[string]any)["triggers"].([]any)
	if len(triggers) != 2 {
		t.Fatalf("expected prometheus+cpu triggers: %+v", triggers)
	}
	if q := triggers[0].(map[string]any)["metadata"].(map[string]any)["query"]; q != "sum(custom_metric)" {
		t.Errorf("query override: %v", q)
	}
	if triggers[1].(map[string]any)["type"] != "cpu" {
		t.Errorf("cpu floor missing: %+v", triggers[1])
	}
}

func TestBuildPrometheusRule(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Backup = &misskeyv1beta1.PostgresBackup{DestinationPath: "s3://bk/misskey"}
	rule := buildPrometheusRule(m)

	if rule.GetName() != "example-alerts" || rule.GetKind() != "PrometheusRule" {
		t.Fatalf("rule identity wrong: %s/%s", rule.GetKind(), rule.GetName())
	}
	group := rule.Object["spec"].(map[string]any)["groups"].([]any)[0].(map[string]any)
	rules := group["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("expected 2 alerts, got %d: %+v", len(rules), rules)
	}
	proxyExpr := rules[0].(map[string]any)["expr"].(string)
	if !strings.Contains(proxyExpr, `service="example-proxy"`) || !strings.Contains(proxyExpr, `namespace="ns"`) {
		t.Errorf("proxy alert must scope to own instance: %s", proxyExpr)
	}
	backupExpr := rules[1].(map[string]any)["expr"].(string)
	// 既定48h=172800秒, verify用podを除外する序数限定regex
	if !strings.Contains(backupExpr, "> 172800") || !strings.Contains(backupExpr, `pod=~"^example-db-[0-9]+$"`) {
		t.Errorf("backup alert expr: %s", backupExpr)
	}

	// backup未設定 → proxyアラートのみ
	m.Spec.Postgres.Backup = nil
	rules = buildPrometheusRule(m).Object["spec"].(map[string]any)["groups"].([]any)[0].(map[string]any)["rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["alert"] != "MisskeyProxy5xxHigh" {
		t.Errorf("expected proxy alert only: %+v", rules)
	}

	// proxy無効+backup無し → nil(空のPrometheusRuleを作らない)
	m.Spec.Proxy.Enabled = boolPtr(false)
	if buildPrometheusRule(m) != nil {
		t.Error("no-target rule must be nil")
	}
}

func TestBuildDBScheduledBackup(t *testing.T) {
	m := newMisskey()
	m.Spec.Postgres.Backup = &misskeyv1beta1.PostgresBackup{
		DestinationPath: "s3://bk/misskey",
		Schedule:        "0 0 3 * * *",
	}
	sb := buildDBScheduledBackup(m)
	if sb.GetName() != "example-db" || sb.GetKind() != "ScheduledBackup" {
		t.Errorf("scheduledbackup identity wrong: %s/%s", sb.GetKind(), sb.GetName())
	}
	spec := sb.Object["spec"].(map[string]any)
	if spec["schedule"] != "0 0 3 * * *" || spec["backupOwnerReference"] != "self" {
		t.Errorf("spec: %+v", spec)
	}
	if cl := spec["cluster"].(map[string]any); cl["name"] != "example-db" {
		t.Errorf("cluster name: %v", cl["name"])
	}
}

func TestPoolerHelpers(t *testing.T) {
	m := newMisskey()
	if poolerEnabled(m) {
		t.Error("pooler unset should be disabled")
	}
	// ポインタ存在=有効(内側Enabledは廃止)
	m.Spec.Postgres.Pooler = &misskeyv1beta1.PostgresPooler{}
	if !poolerEnabled(m) {
		t.Error("pooler block present should be enabled")
	}
}

func TestRedisHAAuth(t *testing.T) {
	// HA redis: requirepass secret参照 + passEnv
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1beta1.RedisHA{}
	ep := resolve(m).redisDefault
	if ep.passSel == nil || ep.passSel.Name != "example-redis-auth" || ep.passSel.Key != "password" {
		t.Errorf("HA redis must carry requirepass secret ref: %+v", ep.passSel)
	}
	if ep.passEnv != "REDIS_PASSWORD" {
		t.Errorf("default HA passEnv: %q", ep.passEnv)
	}
	// standalone managed: requirepass認証あり(NP+認証の多層防御)
	sa := resolve(newMisskey()).redisDefault
	if sa.passSel == nil || sa.passSel.Name != "example-redis-auth" || sa.passEnv != "REDIS_PASSWORD" {
		t.Errorf("standalone managed redis must carry requirepass: sel=%+v env=%q", sa.passSel, sa.passEnv)
	}
	// role HA: role別passEnv
	m2 := newMisskey()
	m2.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{JobQueue: &misskeyv1beta1.RedisRole{HA: &misskeyv1beta1.RedisHA{}}}
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
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{JobQueue: &misskeyv1beta1.RedisRole{}}
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
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{
		Pubsub: &misskeyv1beta1.RedisRole{External: &misskeyv1beta1.ExternalRedis{
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
	m.Spec.Redis.HA = &misskeyv1beta1.RedisHA{}
	ep := resolve(m).redisDefault
	if !ep.managed || !ep.ha || ep.masterName != "mymaster" {
		t.Errorf("HA default endpoint wrong: %+v", ep)
	}
	if len(ep.sentinels) != 1 || ep.sentinels[0].host != "example-redis-ha-sentinel" || ep.sentinels[0].port != 26379 {
		t.Errorf("HA sentinel endpoint wrong: %+v", ep.sentinels)
	}
}

func TestRenderRedisBlockSentinel(t *testing.T) {
	ep := redisEndpoint{host: "h", port: 6379, sentinels: []redisHostPort{{host: "s", port: 26379}}, masterName: "mymaster"}
	var b strings.Builder
	renderRedisBlock(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }, "redis", ep)
	out := b.String()
	for _, s := range []string{"sentinels:", `- host: "s"`, "port: 26379", `name: "mymaster"`} {
		if !strings.Contains(out, s) {
			t.Errorf("sentinel block missing %q\n%s", s, out)
		}
	}
}

func TestRenderDefaultYMLRedisRolesAndHA(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1beta1.RedisHA{}
	// role単位で独立: jobQueueは自分のhaでHA、pubsubはha無しでstandalone
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{
		JobQueue: &misskeyv1beta1.RedisRole{HA: &misskeyv1beta1.RedisHA{}},
		Pubsub:   &misskeyv1beta1.RedisRole{},
	}
	out := renderDefaultYML(m, resolve(m))
	// default redisとjobQueueはHA sentinel、pubsubはstandalone(sentinelなし)
	for _, s := range []string{"redis:", "sentinels:", `name: "mymaster"`, "redisForJobQueue:", `host: "example-redis-jobqueue-ha-sentinel"`, "redisForPubsub:", `host: "example-redis-pubsub"`} {
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
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{
		JobQueue: &misskeyv1beta1.RedisRole{},
		Pubsub:   &misskeyv1beta1.RedisRole{External: &misskeyv1beta1.ExternalRedis{Host: "x"}},
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

func redisInstanceBySuffix(m *misskeyv1beta1.Misskey, suffix string) redisManagedInstance {
	for _, i := range managedRedisInstances(m) {
		if i.suffix == suffix {
			return i
		}
	}
	return redisManagedInstance{}
}

func TestBuildRedisReplicationAndSentinel(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.HA = &misskeyv1beta1.RedisHA{Replicas: 5, Sentinels: 3}
	inst := redisInstanceBySuffix(m, "")

	repl := buildRedisReplication(m, inst)
	if repl.GetKind() != "RedisReplication" || repl.GetName() != "example-redis-ha" {
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
	if rsc["redisReplicationName"] != "example-redis-ha" || rsc["masterGroupName"] != "mymaster" || rsc["quorum"] != "2" {
		t.Errorf("redisSentinelConfig wrong: %+v", rsc)
	}
}

func TestRedisEgressRule(t *testing.T) {
	m := newMisskey()
	if redisEgressRule(m) != nil {
		t.Error("no HA → redisEgressRule nil")
	}
	m.Spec.Redis.HA = &misskeyv1beta1.RedisHA{}
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
	if !has("example-redis-ha") || !has("example-redis-ha-sentinel") {
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
	job := buildMigrationJob(m, resolve(m), nil)
	if envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("default (opt-in) must omit MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY")
	}
	// falseでも同様
	m.Spec.Migration.CreateIndexConcurrently = boolPtr(false)
	job = buildMigrationJob(m, resolve(m), nil)
	if envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("createIndexConcurrently=false must omit the env")
	}
}

func TestMigrationConcurrentOptIn(t *testing.T) {
	m := newMisskey()
	m.Spec.Migration.CreateIndexConcurrently = boolPtr(true)
	job := buildMigrationJob(m, resolve(m), nil)
	if !envHasName(job.Spec.Template.Spec.Containers[0].Env, "MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY") {
		t.Error("createIndexConcurrently=true must set the env on the migration Job")
	}
	// app/workerには付かない(migration専用)
	for _, role := range []string{roleApp, roleWorker} {
		spec := buildMisskeyPodSpec(m, resolve(m), role, m.Spec.App.ComponentSpec)
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
	a := workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 5},
	})
	if !autoscalingEnabled(a) {
		t.Error("present block is enabled")
	}
	if autoscalingUsesKEDA(a) {
		t.Error("no queues → native HPA")
	}
	if !autoscalingUsesKEDA(workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
		Queues: []misskeyv1beta1.QueueScaleTrigger{{Name: "deliver", ListLength: 1}},
	})) {
		t.Error("queues → KEDA")
	}
	if !autoscalingUsesKEDA(appScaleConfig(&misskeyv1beta1.AppAutoscalingSpec{
		RPS: &misskeyv1beta1.RPSTrigger{ServerAddress: "http://prom:9090", TargetRPS: 50},
	})) {
		t.Error("rps → KEDA")
	}
}

func TestStaticReplicas(t *testing.T) {
	comp := misskeyv1beta1.ComponentSpec{Replicas: int32Ptr(3)}
	if r := staticReplicas(comp, nil); r == nil || *r != 3 {
		t.Errorf("no autoscaling → static replicas: %v", r)
	}
	sc := appScaleConfig(&misskeyv1beta1.AppAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 5},
	})
	if staticReplicas(comp, sc) != nil {
		t.Error("autoscaling → replicas unmanaged (nil)")
	}
}

func TestBuildHPASpec(t *testing.T) {
	a := &scaleConfig{AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 10, TargetCPUUtilizationPercentage: int32Ptr(70)}}
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
	def := buildHPASpec("x", &scaleConfig{AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 3}})
	if len(def.Metrics) != 1 || *def.Metrics[0].Resource.Target.AverageUtilization != 80 {
		t.Errorf("default metric must be cpu 80: %+v", def.Metrics)
	}
	// memory
	mem := buildHPASpec("x", &scaleConfig{AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 3, TargetMemoryUtilizationPercentage: int32Ptr(75)}})
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
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{JobQueue: &misskeyv1beta1.RedisRole{}}
	if ep := jobQueueEndpoint(resolve(m)); ep.host != "example-redis-jobqueue" {
		t.Errorf("separated jobQueue endpoint: %q", ep.host)
	}
}

func scaledObjectTriggers(so map[string]any) []any {
	return so["triggers"].([]any)
}

func TestBuildScaledObjectStandalone(t *testing.T) {
	m := newMisskey() // default redis standalone
	a := workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 30},
		Queues:          []misskeyv1beta1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000}, {Name: "inbox", ListLength: 500}},
	})
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
	m.Spec.Redis.Roles = &misskeyv1beta1.RedisRoles{JobQueue: &misskeyv1beta1.RedisRole{HA: &misskeyv1beta1.RedisHA{}}}
	a := workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 20},
		Queues:          []misskeyv1beta1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000, ListName: "custom:deliver:wait"}},
	})
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
	if meta["hosts"] != "example-redis-jobqueue-ha-sentinel.ns.svc" || meta["ports"] != "26379" || meta["sentinelMaster"] != "mymaster" {
		t.Errorf("sentinel meta wrong (hosts/ports): %+v", meta)
	}
	if meta["listName"] != "custom:deliver:wait" {
		t.Errorf("listName override ignored: %v", meta["listName"])
	}
}

func TestBuildScaledObjectExternalRedis(t *testing.T) {
	// external redisはcluster外hostのためFQDN(.ns.svc)化しない
	m := newMisskey()
	m.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{Host: "redis.prod.example.com", Port: 6380, DB: 2}
	a := workerScaleConfig(&misskeyv1beta1.WorkerAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 10},
		Queues:          []misskeyv1beta1.QueueScaleTrigger{{Name: "deliver", ListLength: 1000}},
	})
	so := buildScaledObject(m, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m)))
	trig := scaledObjectTriggers(so.Object["spec"].(map[string]any))[0].(map[string]any)
	meta := trig["metadata"].(map[string]any)
	if meta["address"] != "redis.prod.example.com:6380" {
		t.Errorf("external address must not be FQDN-ified: %v", meta["address"])
	}
	if meta["databaseIndex"] != "2" {
		t.Errorf("external db index must reach the KEDA trigger: %v", meta["databaseIndex"])
	}

	// external sentinelも同様にhostそのまま
	m2 := newMisskey()
	m2.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{
		Host: "redis.prod.example.com", MasterName: "mymaster",
		Sentinels: []misskeyv1beta1.RedisHostPort{{Host: "s1.prod.example.com"}, {Host: "s2.prod.example.com", Port: 26380}},
	}
	so2 := buildScaledObject(m2, roleWorker, "example-worker", a, jobQueueEndpoint(resolve(m2)))
	trig2 := scaledObjectTriggers(so2.Object["spec"].(map[string]any))[0].(map[string]any)
	meta2 := trig2["metadata"].(map[string]any)
	if meta2["hosts"] != "s1.prod.example.com,s2.prod.example.com" || meta2["ports"] != "26379,26380" {
		t.Errorf("external sentinel hosts/ports must not be FQDN-ified: %+v", meta2)
	}
}

func TestRenderExternalRedisDB(t *testing.T) {
	m := newMisskey()
	m.Spec.Redis.External = &misskeyv1beta1.ExternalRedis{Host: "redis.svc", DB: 3}
	if out := renderDefaultYML(m, resolve(m)); !strings.Contains(out, "  db: 3\n") {
		t.Errorf("external redis db index missing:\n%s", out)
	}
	// db=0(Misskey既定)は省略しmanagedのchecksumを変えない
	m2 := newMisskey()
	if strings.Contains(renderDefaultYML(m2, resolve(m2)), "  db: 0\n") {
		t.Error("db: 0 must be omitted")
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

	// proxy: Caddy metricsのServiceMonitor(auth無し)
	psm := buildServiceMonitor(m, "proxy-metrics", "proxy", selectorFor(m, "proxy"), "metrics", "/metrics", nil)
	peps, _, _ := unstructured.NestedSlice(psm.Object, "spec", "endpoints")
	pep0 := peps[0].(map[string]any)
	if pep0["port"] != "metrics" || pep0["path"] != "/metrics" {
		t.Errorf("proxy SM endpoint: %v", pep0)
	}
	if _, ok := pep0["authorization"]; ok {
		t.Error("proxy SM must not carry authorization")
	}
}

func TestProxyMetricsPort(t *testing.T) {
	m := newMisskey()
	// proxyコンテナはmetricsポート(:9180)を公開
	proxyPod := buildCaddyPodSpec(m, true)
	if !hasContainerPort(proxyPod.Containers[0].Ports, proxyMetricsPort) {
		t.Errorf("proxy container missing metrics port %d: %+v", proxyMetricsPort, proxyPod.Containers[0].Ports)
	}
	// maintenance有効時のみHTML CMをマウント
	if !hasVolume(proxyPod.Volumes, "maintenance-html") {
		t.Errorf("proxy pod missing maintenance-html volume: %+v", proxyPod.Volumes)
	}
	if hasVolume(buildCaddyPodSpec(m, false).Volumes, "maintenance-html") {
		t.Errorf("maintenance disabled must not mount maintenance-html")
	}
}

func hasVolume(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasContainerPort(ports []corev1.ContainerPort, p int32) bool {
	for _, cp := range ports {
		if cp.ContainerPort == p {
			return true
		}
	}
	return false
}
