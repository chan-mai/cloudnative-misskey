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
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// yq: ユーザ由来スカラーを安全なYAML二重引用符スカラーへエンコード
// JSONエンコードはYAML1.2のflowスカラーとして妥当で、改行/制御文字を\nへエスケープし
// default.ymlへの構造注入(重複キー・任意キー注入・DoS)を封じる
func yq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// clientIPHeaderPattern: 有効なHTTPヘッダ名(CRD Patternと同一)。非適合はCaddyfileへ出力しない
var clientIPHeaderPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// renderRedisBlock: redis / redisForXxx ブロックを出力
// sentinels非空でSentinelモード(host/portはioredis上無視だがMisskey schema必須)
func renderRedisBlock(w func(string, ...any), key string, ep redisEndpoint) {
	w("%s:\n", key)
	w("  host: %s\n", yq(ep.host))
	w("  port: %d\n", ep.port)
	// 0はMisskey既定のため省略(managedのchecksumを変えない)
	if ep.db != 0 {
		w("  db: %d\n", ep.db)
	}
	if len(ep.sentinels) > 0 {
		w("  sentinels:\n")
		for _, s := range ep.sentinels {
			w("    - host: %s\n", yq(s.host))
			w("      port: %d\n", s.port)
		}
		w("  name: %s\n", yq(ep.masterName))
	}
	if ep.passSel != nil {
		w("  pass: ${%s}\n", ep.passEnv)
		// OT operatorのsentinelもrequirepassを要求するためsentinelPasswordが要る(ioredis passthrough)
		if len(ep.sentinels) > 0 {
			w("  sentinelPassword: ${%s}\n", ep.passEnv)
		}
	}
	// external redisのTLSを実接続へ反映(ioredisのtlsオプション, 空オブジェクトで有効化)
	if ep.enableTLS {
		w("  tls: {}\n")
	}
}

// Misskeyの.config/default.ymlを生成。シークレットは${PLACEHOLDER}トークンのまま残す
// initコンテナがpod起動時に置換するため、シークレット値がConfigMapに載らない
func renderDefaultYML(m *misskeyv1beta1.Misskey, p plan) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Managed by cloudnative-misskey. Do not edit by hand.\n")
	w("url: %s\n", yq(m.Spec.URL))
	w("port: %d\n\n", misskeyPort)

	if p.setupEnabled {
		w("setupPassword: ${SETUP_PASSWORD}\n\n")
	}

	w("db:\n")
	w("  host: %s\n", yq(p.dbHost))
	w("  port: %d\n", p.dbPort)
	w("  db: %s\n", yq(p.dbName))
	w("  user: %s\n", yq(p.dbUser))
	w("  pass: ${DB_PASSWORD}\n")
	// read offload時のみdbReplications+dbSlavesを配線。slaveのpassも同一appの${DB_PASSWORD}
	if p.dbReplications {
		w("dbReplications: true\n")
		w("dbSlaves:\n")
		for _, s := range p.dbSlaves {
			w("  - host: %s\n", yq(s.host))
			w("    port: %d\n", s.port)
			w("    db: %s\n", yq(s.db))
			w("    user: %s\n", yq(s.user))
			w("    pass: ${DB_PASSWORD}\n")
		}
		w("\n")
	} else {
		w("dbReplications: false\n\n")
	}

	renderRedisBlock(w, "redis", p.redisDefault)
	// 分離されたroleのみredisForXxxを出力。未分離roleは省略しMisskeyがredisにfallback
	for _, rd := range redisRoleDescs {
		if ep, ok := p.redisRoles[rd.key]; ok {
			renderRedisBlock(w, rd.configKey, ep)
		}
	}
	w("\n")

	w("fulltextSearch:\n")
	w("  provider: %s\n", p.provider)
	if p.meiliEnabled {
		w("\nmeilisearch:\n")
		w("  host: %s\n", yq(p.meiliHost))
		w("  port: %d\n", p.meiliPort)
		w("  apiKey: ${MEILI_KEY}\n")
		w("  ssl: %s\n", strconv.FormatBool(p.meiliSSL))
		w("  index: %s\n", yq(p.meiliIndex))
		w("  scope: %s\n", p.meiliScope)
	}
	w("\n")

	w("id: '%s'\n", stringOr(m.Spec.IDGenerationMethod, "aidx"))
	w("proxyRemoteFiles: %s\n", strconv.FormatBool(boolOr(m.Spec.Files.ProxyRemoteFiles, true)))
	w("signToActivityPubGet: true\n")

	// 第一級化した任意knobs(未設定キーは出力せずMisskey既定に委ねる)
	renderPerformance(w, m.Spec.Performance)
	renderOutboundProxy(w, m.Spec.OutboundProxy)
	renderFiles(w, m.Spec.Files)

	if strings.TrimSpace(m.Spec.ExtraConfig) != "" {
		w("\n# --- extraConfig ---\n%s\n", strings.TrimRight(m.Spec.ExtraConfig, "\n"))
	}
	return b.String()
}

// renderPerformance: job-queue knobsをdefault.ymlへ(未設定は出力しない=Misskey既定に委ねる)
func renderPerformance(w func(string, ...any), p misskeyv1beta1.PerformanceSpec) {
	writeInt := func(key string, v *int32) {
		if v != nil {
			w("%s: %d\n", key, *v)
		}
	}
	writeInt("deliverJobConcurrency", p.DeliverJobConcurrency)
	writeInt("inboxJobConcurrency", p.InboxJobConcurrency)
	writeInt("deliverJobPerSec", p.DeliverJobPerSec)
	writeInt("inboxJobPerSec", p.InboxJobPerSec)
	writeInt("relationshipJobPerSec", p.RelationshipJobPerSec)
	writeInt("deliverJobMaxAttempts", p.DeliverJobMaxAttempts)
	writeInt("inboxJobMaxAttempts", p.InboxJobMaxAttempts)
}

// renderOutboundProxy: 外向きforward proxy設定(未設定キーは出力しない)
func renderOutboundProxy(w func(string, ...any), o misskeyv1beta1.OutboundProxySpec) {
	if o.HTTP != "" {
		w("proxy: %s\n", yq(o.HTTP))
	}
	if o.SMTP != "" {
		w("proxySmtp: %s\n", yq(o.SMTP))
	}
	if len(o.BypassHosts) > 0 {
		quoted := make([]string, len(o.BypassHosts))
		for i, h := range o.BypassHosts {
			quoted[i] = yq(h)
		}
		w("proxyBypassHosts: [%s]\n", strings.Join(quoted, ", "))
	}
}

// renderFiles: media/file設定(未設定キーは出力しない)。proxyRemoteFilesは既定出力のため別処理
func renderFiles(w func(string, ...any), f misskeyv1beta1.FilesSpec) {
	if f.MaxFileSize != nil {
		w("maxFileSize: %d\n", *f.MaxFileSize)
	}
	if f.MediaProxy != "" {
		w("mediaProxy: %s\n", yq(f.MediaProxy))
	}
}

// migratePlan: migration用にpを複製し、DB経路をprimary直結・no-replicationへ倒す
// managedではpooler(-pooler-rw)を迂回し-rwへ。externalはhostそのまま
func migratePlan(m *misskeyv1beta1.Misskey, p plan) plan {
	mp := p
	if p.dbManaged {
		mp.dbHost = nameDBService(m)
	}
	mp.dbReplications = false
	mp.dbSlaves = nil
	return mp
}

// appの前段に置くリバースプロキシのCaddyfileを生成
func renderCaddyfile(m *misskeyv1beta1.Misskey) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s) }

	// ingress(private range)のX-Forwarded-*信頼用グローバルオプション
	// 未設定時はCaddyが上書きしclient IP・https喪失
	// 前段が非privateなら実CIDR調整
	// metricsでper-handler HTTPメトリクス(リクエスト数/レイテンシ/ステータス)を収集
	w("{\n")
	w("\tservers {\n")
	w("\t\ttrusted_proxies static private_ranges\n")
	w("\t\tmetrics\n")
	w("\t}\n")
	w("}\n\n")
	// metrics専用リスナ。admin APIは露出させず/metricsのみ配信(Prometheusがscrape)
	w(fmt.Sprintf(":%d {\n\tmetrics /metrics\n}\n\n", proxyMetricsPort))
	w(fmt.Sprintf(":%d {\n", proxyPort))
	w("\tencode gzip\n")
	w("\theader /assets Cache-Control \"public, max-age=31536000, immutable\"\n\n")
	w(fmt.Sprintf("\treverse_proxy %s:%d {\n", nameApp(m), misskeyPort))
	// ClientIPHeader指定時のみX-Real-IP/X-Forwarded-Forを上書き
	// 未指定時はtrusted_proxiesで信頼したupstreamのX-Forwarded-Forを保持
	if h := m.Spec.Proxy.ClientIPHeader; h != "" && clientIPHeaderPattern.MatchString(h) {
		w(fmt.Sprintf("\t\theader_up X-Real-IP {header.%s}\n", h))
		w(fmt.Sprintf("\t\theader_up X-Forwarded-For {header.%s}\n", h))
	}
	// X-Forwarded-Protoは上書きせず、trusted_proxiesでupstreamのhttpsを保持
	w("\t\theader_up X-Forwarded-Host {host}\n\n")
	w("\t\thealth_uri /api/server-info\n")
	w("\t\thealth_interval 10s\n")
	w("\t\thealth_timeout 2s\n")
	w("\t\thealth_status 200\n")
	w("\t}\n")

	if boolOr(m.Spec.Proxy.Maintenance.Enabled, true) {
		status := int32(200)
		if sc := m.Spec.Proxy.Maintenance.StatusCode; sc != nil {
			status = *sc
		}
		w("\n\thandle_errors {\n")
		w("\t\t@backend_down `{err.status_code} in [500, 501, 502, 503, 504, 522]`\n")
		w("\t\thandle @backend_down {\n")
		// /api/*はメンテナンスページから除外し、外部ヘルスチェックが2xxのメンテ応答でなく実際のバックエンドステータスを見られるようにする
		w("\t\t\t@api path /api/*\n")
		w("\t\t\thandle @api {\n")
		w("\t\t\t\trespond \"\" {err.status_code}\n")
		w("\t\t\t}\n")
		// メンテページはproxy自身のfile_serverで配信(既定200, /api/*は上記で実status)
		w("\t\t\thandle {\n")
		w("\t\t\t\theader Cache-Control \"no-store\"\n")
		w("\t\t\t\troot * /usr/share/caddy\n")
		w("\t\t\t\trewrite * /maintenance.html\n")
		w("\t\t\t\tfile_server {\n")
		w(fmt.Sprintf("\t\t\t\t\tstatus %d\n", status))
		w("\t\t\t\t}\n")
		w("\t\t\t}\n")
		w("\t\t}\n")
		w("\t}\n")
	}
	w("}\n")
	return b.String()
}

// spec.proxy.maintenance.htmlが空の時に配信するページを生成
// reloadSeconds>0ならページ再読込スクリプトを埋め込み、バックエンド復帰後に訪問者が自動で戻る
func defaultMaintenanceHTML(reloadSeconds int32) string {
	reload := ""
	if reloadSeconds > 0 {
		reload = fmt.Sprintf("\n<script>setTimeout(function () { location.reload() }, %d)</script>", reloadSeconds*1000)
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Under maintenance</title>` + reload + `
<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#f5f5f7;color:#333}main{text-align:center}</style>
</head>
<body><main><h1>Under maintenance</h1><p>This server is temporarily unavailable. Please try again shortly.</p></main></body>
</html>
`
}

// メンテナンスページ本文を返す。ユーザ指定があればそれ、なければ設定した自動再読込間隔の組込みページ
func maintenanceHTMLContent(m *misskeyv1beta1.Misskey) string {
	reload := int32(30)
	if r := m.Spec.Proxy.Maintenance.ReloadSeconds; r != nil {
		reload = *r
	}
	return stringOr(m.Spec.Proxy.Maintenance.HTML, defaultMaintenanceHTML(reload))
}

// config ConfigMap(default.yml + Caddyfile)を作成/更新し、プロキシ有効時はメンテナンスHTMLのConfigMapも扱う
func (r *MisskeyReconciler) reconcileConfigMaps(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameConfig(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, cm, func() error {
		cm.Labels = labelsFor(m, "config")
		cm.Data = map[string]string{
			"default.yml": renderDefaultYML(m, p),
		}
		if boolOr(m.Spec.Proxy.Enabled, true) {
			cm.Data["Caddyfile"] = renderCaddyfile(m)
		}
		return nil
	}); err != nil {
		return err
	}

	// migration専用config: pooler/replicaを迂回しprimary(-rw)へ直結、dbReplications無効
	// poolMode=transactionのPgBouncer越しやreplica lagにmigration DDLを晒さない不変条件
	migrateCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameMigrateConfig(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, migrateCM, func() error {
		migrateCM.Labels = labelsFor(m, "config")
		migrateCM.Data = map[string]string{
			"default.yml": renderDefaultYML(m, migratePlan(m, p)),
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
			"maintenance.html": maintenanceHTMLContent(m),
		}
		return nil
	})
}

// operator管理のsetupパスワードSecretの存在を保証
// spec.setupPasswordがありsecretRefがない時のみ作成。生成後のパスワードは上書きしない
func (r *MisskeyReconciler) reconcileSetupSecret(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nameSetup(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, secret, func() error {
		secret.Labels = labelsFor(m, "setup")
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data[setupPasswordID]; !ok || rotationRequested(m, secret) {
			pw, err := randomHex(16)
			if err != nil {
				return err
			}
			secret.Data[setupPasswordID] = []byte(pw)
		}
		markRotation(m, secret)
		return nil
	})
}

// このコントローラをfield managerとしてobjをserver-side apply
// applyのread-modify-write全置換と違い、SSAは設定したフィールドのみマージし対象コントローラ所有のフィールドは保持(例: CNPGのwebhookがClusterに付与する既定値)。resync毎の差分を防ぐ
func (r *MisskeyReconciler) applySSA(ctx context.Context, m *misskeyv1beta1.Misskey, obj *unstructured.Unstructured) error {
	if err := controllerutil.SetControllerReference(m, obj, r.Scheme); err != nil {
		return err
	}
	return r.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner("cloudnative-misskey"), client.ForceOwnership)
}

// controller owner referenceも刻む薄いCreateOrUpdateラッパ。子はMisskeyオブジェクトと共にGCされる
func (r *MisskeyReconciler) apply(ctx context.Context, m *misskeyv1beta1.Misskey, obj client.Object, mutate func() error) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(m, obj, r.Scheme)
	})
	return err
}
