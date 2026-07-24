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
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// dbEndpoint: dbSlave1件分の接続先(pass は常に${DB_PASSWORD})
type dbEndpoint struct {
	host string
	port int32
	db   string
	user string
}

// redisHostPort: sentinelエンドポイント1件
type redisHostPort struct {
	host string
	port int32
}

// redisEndpoint: 1つのredis接続先(default or role)。sentinels非空でSentinelモード
type redisEndpoint struct {
	host       string
	port       int32
	db         int32 // 論理DB index。managedは常に0
	sentinels  []redisHostPort
	masterName string
	passSel    *corev1.SecretKeySelector // 認証secret。nilなら認証なし
	passEnv    string                    // passSel使用時のプレースホルダenv名(REDIS_PASSWORD / REDIS_PASSWORD_<ROLE>)
	managed    bool                      // operatorが建てる
	ha         bool                      // Sentinel HAモード
	enableTLS  bool                      // external redisのTLS(KEDA triggerへ伝播)
}

// 1インスタンス分の解決済み接続パラメータを保持
// managed/externalの分岐を平坦化し、ビルダー側で分岐不要にする
type plan struct {
	// Postgres
	dbManaged      bool
	dbHost         string
	dbPort         int32
	dbName         string
	dbUser         string
	dbPassSel      corev1.SecretKeySelector
	dbReplications bool         // Misskeyのread offload有効
	dbSlaves       []dbEndpoint // read replica(またはroプーラー)接続先

	// Redis
	redisDefault redisEndpoint            // 共有redis(redis:ブロック)
	redisRoles   map[string]redisEndpoint // 分離roleのみ(key=redisRoleDesc.key)

	// MeiliSearch/検索
	provider     misskeyv1beta1.SearchProvider
	meiliEnabled bool // provider == meilisearch
	meiliManaged bool
	meiliHost    string
	meiliPort    int32
	meiliSSL     bool
	meiliIndex   string
	meiliScope   string
	meiliKeySel  corev1.SecretKeySelector

	// Ingress
	ingressHost string

	// setupパスワード
	setupEnabled bool
	setupManaged bool // operatorがSecretを生成
	setupSel     corev1.SecretKeySelector

	// objectStorage(S3/R2 media)
	objEnabled        bool // spec.objectStorage != nil
	objAutoConfigure  bool // objEnabled かつ autoConfigure(既定true)。falseならoperatorはmeta非管理
	objBucket         string
	objEndpoint       string
	objRegion         string
	objPrefix         string
	objBaseURL        string
	objPort           *int32
	objUseSSL         bool
	objUseProxy       bool
	objSetPublicRead  bool
	objForcePathStyle bool
	objAccessKeySel   corev1.SecretKeySelector
	objSecretKeySel   corev1.SecretKeySelector
	objColumns        map[string]string // logical key -> 解決済みカラム名
	objExtra          map[string]string // extraカラム名 -> 平文値
	objImage          string
}

// specを既定値適用の上でplanに平坦化
func resolve(m *misskeyv1beta1.Misskey) plan {
	p := plan{}

	// --- PostgreSQL ---
	if ext := m.Spec.Postgres.External; ext != nil {
		p.dbManaged = false
		p.dbHost = ext.Host
		p.dbPort = int32OrDefault(ext.Port, postgresPort)
		p.dbName = ext.Database
		p.dbUser = ext.User
		p.dbPassSel = ext.PasswordSecret
	} else {
		p.dbManaged = true
		p.dbPort = postgresPort
		p.dbName = stringOr(m.Spec.Postgres.Database, "misskey")
		p.dbUser = stringOr(m.Spec.Postgres.Owner, "misskey")
		// CNPGがapp secret <cluster>-appをusername/passwordキーで生成
		p.dbPassSel = corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: nameDBAppSecret(m)},
			Key:                  "password",
		}

		// pooler有効ならwrite/read経路をPgBouncerサービスへ、無効ならCNPGのrw/roサービスへ
		usePooler := poolerEnabled(m)
		if usePooler {
			p.dbHost = nameDBPoolerRW(m)
		} else {
			p.dbHost = nameDBService(m)
		}

		// read offload: replicaが居る(instances>=2)時に自動オン。readOffload:falseでopt-out
		if readOffloadActive(m) {
			readHost := nameDBReadService(m)
			if usePooler {
				readHost = nameDBPoolerRO(m)
			}
			p.dbReplications = true
			p.dbSlaves = []dbEndpoint{{host: readHost, port: postgresPort, db: p.dbName, user: p.dbUser}}
		}
	}

	// --- Redis ---
	p.redisRoles = map[string]redisEndpoint{}
	defaultHA := m.Spec.Redis.HA
	if ext := m.Spec.Redis.External; ext != nil {
		p.redisDefault = externalRedisEndpoint(ext, "REDIS_PASSWORD")
	} else {
		p.redisDefault = managedRedisEndpoint(m, "", "REDIS_PASSWORD", defaultHA)
	}
	if roles := m.Spec.Redis.Roles; roles != nil {
		for _, rd := range redisRoleDescs {
			role := rd.get(roles)
			if role == nil {
				continue // 未分離roleはredis:にfallback(ブロックを出さない)
			}
			if role.External != nil {
				p.redisRoles[rd.key] = externalRedisEndpoint(role.External, rd.passEnv)
			} else {
				// role単位で独立(role.ha存在=HA)。redis.haの継承はしない
				p.redisRoles[rd.key] = managedRedisEndpoint(m, rd.nameSuffix, rd.passEnv, role.HA)
			}
		}
	}

	// --- Search ---
	p.provider = misskeyv1beta1.SearchProvider(stringOr(string(m.Spec.Search.Provider), string(misskeyv1beta1.SearchMeilisearch)))
	if p.provider == misskeyv1beta1.SearchMeilisearch {
		p.meiliEnabled = true
		ms := m.Spec.Search.Meilisearch
		p.meiliIndex = stringOr(ms.Index, sanitizeIndex(hostFromURL(m.Spec.URL)))
		p.meiliScope = stringOr(ms.Scope, "local")
		if ext := ms.External; ext != nil {
			p.meiliManaged = false
			p.meiliHost = ext.Host
			p.meiliPort = int32OrDefault(ext.Port, meiliPort)
			p.meiliSSL = ext.SSL
			p.meiliKeySel = ext.APIKeySecret
		} else {
			p.meiliManaged = true
			p.meiliHost = nameMeili(m)
			p.meiliPort = meiliPort
			p.meiliSSL = false
			if ms.MasterKeySecret != nil {
				p.meiliKeySel = *ms.MasterKeySecret
			} else {
				p.meiliKeySel = corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: nameMeili(m)},
					Key:                  meiliMasterKeyID,
				}
			}
		}
	}

	// --- ObjectStorage ---
	if os := m.Spec.ObjectStorage; os != nil {
		p.objEnabled = true
		p.objAutoConfigure = boolOr(os.AutoConfigure, true)
		p.objBucket = os.Bucket
		p.objEndpoint = os.Endpoint
		// リージョン概念がない場合の既定。空リージョンはAWS SDKが"Region is missing"で弾く
		p.objRegion = stringOr(os.Region, "us-east-1")
		p.objPrefix = os.Prefix
		p.objBaseURL = os.BaseURL
		p.objPort = os.Port
		p.objUseSSL = boolOr(os.UseSSL, true)
		p.objUseProxy = boolOr(os.UseProxy, true)
		p.objSetPublicRead = boolOr(os.SetPublicRead, false)
		p.objForcePathStyle = boolOr(os.S3ForcePathStyle, true)
		p.objAccessKeySel = os.Credentials.AccessKeyID
		p.objSecretKeySel = os.Credentials.SecretAccessKey
		p.objColumns = objectStorageColumns(os.ColumnNames)
		p.objExtra = os.ExtraColumns
		p.objImage = stringOr(os.Image, "ghcr.io/cloudnative-pg/postgresql:17")
	}

	// --- Ingress host ---
	p.ingressHost = stringOr(m.Spec.Ingress.Host, hostFromURL(m.Spec.URL))

	// --- Setup password ---
	if sp := m.Spec.SetupPassword; sp != nil {
		p.setupEnabled = true
		if sp.SecretRef != nil {
			p.setupSel = *sp.SecretRef
		} else {
			p.setupManaged = true
			p.setupSel = corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: nameSetup(m)},
				Key:                  setupPasswordID,
			}
		}
	}

	return p
}

// objectStorageColumnDefaults: metaテーブルのupstream既定カラム名(logical key -> 列名)
var objectStorageColumnDefaults = map[string]string{
	"useObjectStorage": "useObjectStorage",
	"baseUrl":          "objectStorageBaseUrl",
	"bucket":           "objectStorageBucket",
	"prefix":           "objectStoragePrefix",
	"endpoint":         "objectStorageEndpoint",
	"region":           "objectStorageRegion",
	"port":             "objectStoragePort",
	"accessKey":        "objectStorageAccessKey",
	"secretKey":        "objectStorageSecretKey",
	"useSSL":           "objectStorageUseSSL",
	"useProxy":         "objectStorageUseProxy",
	"setPublicRead":    "objectStorageSetPublicRead",
	"s3ForcePathStyle": "objectStorageS3ForcePathStyle",
}

// objectStorageColumns: 既定カラム名にspec.columnNamesの上書きをマージ(fork/旧version対応)
func objectStorageColumns(c *misskeyv1beta1.ObjectStorageColumns) map[string]string {
	out := make(map[string]string, len(objectStorageColumnDefaults))
	for k, v := range objectStorageColumnDefaults {
		out[k] = v
	}
	if c == nil {
		return out
	}
	for logical, override := range map[string]string{
		"useObjectStorage": c.UseObjectStorage,
		"baseUrl":          c.BaseURL,
		"bucket":           c.Bucket,
		"prefix":           c.Prefix,
		"endpoint":         c.Endpoint,
		"region":           c.Region,
		"port":             c.Port,
		"accessKey":        c.AccessKey,
		"secretKey":        c.SecretKey,
		"useSSL":           c.UseSSL,
		"useProxy":         c.UseProxy,
		"setPublicRead":    c.SetPublicRead,
		"s3ForcePathStyle": c.S3ForcePathStyle,
	} {
		if override != "" {
			out[logical] = override
		}
	}
	return out
}

// externalRedisEndpoint: ExternalRedisをendpointへ。Sentinels指定でSentinelモード
func externalRedisEndpoint(ext *misskeyv1beta1.ExternalRedis, passEnv string) redisEndpoint {
	ep := redisEndpoint{
		host:      ext.Host,
		port:      int32OrDefault(ext.Port, redisPort),
		db:        ext.DB,
		passSel:   ext.PasswordSecret,
		passEnv:   passEnv,
		enableTLS: boolOr(ext.TLS, false),
	}
	for _, s := range ext.Sentinels {
		ep.sentinels = append(ep.sentinels, redisHostPort{host: s.Host, port: int32OrDefault(s.Port, sentinelPort)})
	}
	if len(ep.sentinels) > 0 {
		ep.masterName = ext.MasterName
	}
	return ep
}

// managedRedisEndpoint: operator管理redisのendpoint。HA有効でSentinelモード(sentinels+masterName)
// standalone/HAともrequirepass認証あり(passSel=<name>-redis-auth)。NP+認証の多層防御
func managedRedisEndpoint(m *misskeyv1beta1.Misskey, suffix, passEnv string, ha *misskeyv1beta1.RedisHA) redisEndpoint {
	authSel := redisAuthSecretKeySelector(m)
	if ha != nil {
		return redisEndpoint{
			// host/portはioredisのsentinelモードでは無視されるがMisskeyのschema上必須
			host:       nameRedisHA(m, suffix),
			port:       redisPort,
			sentinels:  []redisHostPort{{host: nameRedisSentinelService(m, suffix), port: sentinelPort}},
			masterName: redisMasterGroup,
			passSel:    &authSel,
			passEnv:    passEnv,
			managed:    true,
			ha:         true,
		}
	}
	return redisEndpoint{host: nameRedisInstance(m, suffix), port: redisPort, passSel: &authSel, passEnv: passEnv, managed: true}
}

// vがゼロならdef、そうでなければvを返す
func int32OrDefault(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// URLからホスト部を抽出。不正入力も許容
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// scheme/pathをbest-effortで除去してフォールバック
		s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
		if i := strings.IndexAny(s, "/:"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return u.Hostname()
}

// ホストをMeiliSearchで安全なインデックス名に変換(英数字・ハイフン・アンダースコアのみ)
func sanitizeIndex(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "misskey"
	}
	return out
}
