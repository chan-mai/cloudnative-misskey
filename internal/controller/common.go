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
	"crypto/sha256"
	"encoding/hex"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// ワークロードが参照する描画済みconfigのハッシュを保持
// podテンプレートに刻みconfig変更でpodをローリング。ConfigMap単体はpod起動時のみ読み込みのため
const configChecksumAnnotation = "cloudnative-misskey.dev/config-checksum"

// 指定configパーツのpodテンプレートannotationを返す
func checksumAnnotation(parts ...string) map[string]string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return map[string]string{configChecksumAnnotation: hex.EncodeToString(h.Sum(nil))}
}

// referencedSecretVersions: 描画済みconfigが参照する全Secretのname:resourceVersionを決定的順序で返す
// checksumAnnotationに混ぜてSecret値のローテーションでpodをローリングさせる(不在はname:missing)
// 参照集合はrenderInitEnvと同一。値でなくresourceVersion基準なのは、annotation経由の
// 低エントロピーパスワードのオフライン総当りを避けるため
func (r *MisskeyReconciler) referencedSecretVersions(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) ([]string, error) {
	names := referencedSecretNames(p)
	out := make([]string, 0, len(names))
	for name := range names {
		v, err := r.secretVersion(ctx, m.Namespace, name)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// secretVersion: "name:resourceVersion"を返す。不在(NotFound)のみ"name:missing"、
// その他のerror(Forbidden/API障害等)は伝播する。transientなerrorを"missing"にすると
// checksumが変わってワークロードを不要にroll→復旧時に再rollするため、呼び出し元でreconcileをretryさせる
func (r *MisskeyReconciler) secretVersion(ctx context.Context, ns, name string) (string, error) {
	s := &corev1.Secret{}
	// SecretはClientキャッシュ無効(DisableFor)のためAPI直読(RBAC getのみで可)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, s)
	if err == nil {
		return name + ":" + s.ResourceVersion, nil
	}
	if apierrors.IsNotFound(err) {
		return name + ":missing", nil
	}
	return "", err
}

// referencedSecretNames: 描画済みconfigが参照する全Secret名の集合(値でなく名前のみ)
// Secret watchの絞り込み(参照Secretのみreconcile)にも使う
func referencedSecretNames(p plan) map[string]bool {
	names := map[string]bool{p.dbPassSel.Name: true}
	if p.meiliEnabled {
		names[p.meiliKeySel.Name] = true
	}
	if p.redisDefault.passSel != nil {
		names[p.redisDefault.passSel.Name] = true
	}
	for _, rd := range redisRoleDescs {
		if ep, ok := p.redisRoles[rd.key]; ok && ep.passSel != nil {
			names[ep.passSel.Name] = true
		}
	}
	if p.setupEnabled {
		names[p.setupSel.Name] = true
	}
	return names
}

// rotateAnnotation: CRに付与するとoperator生成Secret(setup/meili/redis-auth)を再生成する
// 値を変えるたびに新しい乱数へローテーション。config-checksum経由でpodがローリングし新値を取り込む
const rotateAnnotation = "cloudnative-misskey.dev/rotate"

// rotationRequested: CRのrotate指示値が既存secretの記録と異なれば再生成すべき
func rotationRequested(m *misskeyv1beta1.Misskey, secret *corev1.Secret) bool {
	want := m.Annotations[rotateAnnotation]
	if want == "" {
		return false
	}
	return secret.Annotations[rotateAnnotation] != want
}

// markRotation: 再生成後にsecretへrotate指示値を記録
func markRotation(m *misskeyv1beta1.Misskey, secret *corev1.Secret) {
	want := m.Annotations[rotateAnnotation]
	if want == "" {
		return
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[rotateAnnotation] = want
}

// インスタンス全体で使う既知のポート番号
const (
	misskeyPort      = 3000
	proxyPort        = 8080
	proxyMetricsPort = 9180
	redisPort        = 6379
	sentinelPort     = 26379
	meiliPort        = 7700
	postgresPort     = 5432
	meiliMasterKeyID = "MEILI_MASTER_KEY"
	setupPasswordID  = "SETUP_PASSWORD"
	// HA時にRedisSentinelが監視するmaster group名。各roleは独立したsentinel群のため共通で可
	redisMasterGroup = "mymaster"

	// 静的バイナリでイメージ自身のファイル所有権に依存しないワークロード用の非rootのuid(Caddy, MeiliSearch)
	// app/workerはMisskeyイメージの実uidで動作
	genericNonRootUID = 1000
)

// インスタンスの1コンポーネント用の標準ラベルセットを返す
func labelsFor(m *misskeyv1beta1.Misskey, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":         "misskey",
		"app.kubernetes.io/instance":     m.Name,
		"app.kubernetes.io/component":    component,
		"app.kubernetes.io/managed-by":   "cloudnative-misskey",
		"cloudnative-misskey.dev/tenant": tenantOf(m),
	}
}

// tenantOf: spec.tenant、未設定ならnamespace
func tenantOf(m *misskeyv1beta1.Misskey) string {
	return stringOr(m.Spec.Tenant, m.Namespace)
}

// コンポーネント用の最小・不変なラベルセレクタを返す
// labelsForより小さく保ち、ラベル変更でセレクタが壊れないようにする
func selectorFor(m *misskeyv1beta1.Misskey, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance":  m.Name,
		"app.kubernetes.io/component": component,
	}
}

// 子オブジェクト名。すべてインスタンス名から決定的に導出
func nameApp(m *misskeyv1beta1.Misskey) string    { return m.Name + "-app" }
func nameWorker(m *misskeyv1beta1.Misskey) string { return m.Name + "-worker" }
func nameProxy(m *misskeyv1beta1.Misskey) string  { return m.Name + "-proxy" }

// nameMaintenance: 統合前構成の掃除専用(deleteLegacyMaintenanceWorkload), 数リリース後に削除可
func nameMaintenance(m *misskeyv1beta1.Misskey) string { return m.Name + "-maintenance" }
func nameRedis(m *misskeyv1beta1.Misskey) string       { return m.Name + "-redis" }

// redis instance名。suffix=""はdefault(<name>-redis)、role時は<name>-redis-<suffix>
func nameRedisInstance(m *misskeyv1beta1.Misskey, suffix string) string {
	if suffix == "" {
		return nameRedis(m)
	}
	return nameRedis(m) + "-" + suffix
}

// HA replication/sentinelのCR名, standalone STS(<name>-redis)とOT作成STS名の衝突回避で-ha基底
func nameRedisHA(m *misskeyv1beta1.Misskey, suffix string) string {
	return nameRedisInstance(m, suffix) + "-ha"
}

// HA時のsentinel Service。OT operatorが<replication名>-sentinelで生成
func nameRedisSentinelService(m *misskeyv1beta1.Misskey, suffix string) string {
	return nameRedisHA(m, suffix) + "-sentinel"
}

// redisコンポーネントlabel。default="redis"、role="redis-<suffix>"
func redisComponent(suffix string) string {
	if suffix == "" {
		return "redis"
	}
	return "redis-" + suffix
}

// Misskeyの役割別Redis記述子。役割走査の単一ソース
// key=CRDフィールド、nameSuffix=k8s名/component、configKey=default.yml、passEnv=external password env
type redisRoleDesc struct {
	key        string
	nameSuffix string
	configKey  string
	passEnv    string
	get        func(*misskeyv1beta1.RedisRoles) *misskeyv1beta1.RedisRole
}

var redisRoleDescs = []redisRoleDesc{
	{"jobQueue", "jobqueue", "redisForJobQueue", "REDIS_PASSWORD_JOBQUEUE", func(r *misskeyv1beta1.RedisRoles) *misskeyv1beta1.RedisRole { return r.JobQueue }},
	{"pubsub", "pubsub", "redisForPubsub", "REDIS_PASSWORD_PUBSUB", func(r *misskeyv1beta1.RedisRoles) *misskeyv1beta1.RedisRole { return r.Pubsub }},
	{"timelines", "timelines", "redisForTimelines", "REDIS_PASSWORD_TIMELINES", func(r *misskeyv1beta1.RedisRoles) *misskeyv1beta1.RedisRole { return r.Timelines }},
	{"reactions", "reactions", "redisForReactions", "REDIS_PASSWORD_REACTIONS", func(r *misskeyv1beta1.RedisRoles) *misskeyv1beta1.RedisRole { return r.Reactions }},
}

func nameMeili(m *misskeyv1beta1.Misskey) string           { return m.Name + "-meilisearch" }
func nameDB(m *misskeyv1beta1.Misskey) string              { return m.Name + "-db" }
func nameConfig(m *misskeyv1beta1.Misskey) string          { return m.Name + "-config" }
func nameMaintenanceHTML(m *misskeyv1beta1.Misskey) string { return m.Name + "-maintenance-html" }
func nameSetup(m *misskeyv1beta1.Misskey) string           { return m.Name + "-setup" }

// version-scopedなmigration Job名。image変更で別Job
func nameMigrate(m *misskeyv1beta1.Misskey) string {
	return m.Name + "-migrate-" + imageHash(m.Spec.Image)
}

// image文字列のsha256先頭10hex
func imageHash(image string) string {
	h := sha256.Sum256([]byte(image))
	return hex.EncodeToString(h[:])[:10]
}

// バックアップ復元検証用の使い捨てCNPG Cluster名
func nameDBVerify(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-verify" }

// CNPGが生成するクラスタのread-writeサービス
func nameDBService(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-rw" }

// CNPGが生成するクラスタのread-onlyサービス(standby replicaへLB)
func nameDBReadService(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-ro" }

// CNPG PgBouncer poolerの名前(=生成Service名)。rw=書込経路、ro=読取経路
func nameDBPoolerRW(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-pooler-rw" }
func nameDBPoolerRO(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-pooler-ro" }

// migration専用のconfig ConfigMap。migrationはprimary直結・no-replicationで別config
func nameMigrateConfig(m *misskeyv1beta1.Misskey) string { return m.Name + "-migrate-config" }

// version-scopedなpre-migration Backup名。migration Jobと同じくimage変更で別Backup
func namePreBackup(m *misskeyv1beta1.Misskey) string {
	return m.Name + "-premigrate-" + imageHash(m.Spec.Image)
}

// HA redisのrequirepass用にoperatorが生成するSecret(全managed HAインスタンス共通)
func nameRedisAuthSecret(m *misskeyv1beta1.Misskey) string { return m.Name + "-redis-auth" }

// CNPGが生成するクラスタのアプリ認証情報Secret
func nameDBAppSecret(m *misskeyv1beta1.Misskey) string { return nameDB(m) + "-app" }

// objectStorage meta書込Job名。入力hash先頭10hexで、設定変更時に別Jobとなる
func nameObjectStorage(m *misskeyv1beta1.Misskey, hash string) string {
	return m.Name + "-objstorage-" + hash[:10]
}

// objectStorage投入SQLのConfigMap(stable名)
func nameObjectStorageSQL(m *misskeyv1beta1.Misskey) string { return m.Name + "-objstorage-sql" }

// vへのポインタを返す
func int32Ptr(v int32) *int32 { return &v }

// vへのポインタを返す
func int64Ptr(v int64) *int64 { return &v }

// vへのポインタを返す
func boolPtr(v bool) *bool { return &v }

// pがnilならdef、そうでなければ*pを返す
func replicasOr(p *int32, def int32) *int32 {
	if p == nil {
		return int32Ptr(def)
	}
	return p
}

// pがnilならdef、そうでなければ*pを返す
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// sが空ならdef、そうでなければsを返す
func stringOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// qがゼロならdefをパースした値、そうでなければqを返す
func quantityOr(q resource.Quantity, def string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(def)
	}
	return q
}

// request/limitが1つでも設定済みならr、なければ既定値を返す
// 管理コンポーネントに下限を与え、resources省略時にBestEffortにしない
func resourcesOr(r corev1.ResourceRequirements, reqCPU, reqMem, limMem string) corev1.ResourceRequirements {
	if len(r.Requests) > 0 || len(r.Limits) > 0 {
		return r
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(reqCPU),
			corev1.ResourceMemory: resource.MustParse(reqMem),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(limMem),
		},
	}
}

// operatorが管理する全ワークロードで再利用する堅牢なpodレベルsecurityContext
func nonRootPodSecurityContext(uid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(uid),
		FSGroup:        int64Ptr(uid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// 堅牢なcontainerレベルのsecurityContext
// readOnlyRootFilesystemはコンテナのrootfsをread-onlyにする。書込先はemptyDir(/tmp等)へ退避
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// tmpVolume/tmpMount: readOnlyRootFilesystem下で/tmpを書込可能にするemptyDir
func tmpVolume() corev1.Volume {
	return corev1.Volume{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func tmpMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}
}

// レプリカをノード間にbest-effortで分散
func spreadConstraints(matchLabels map[string]string) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}
