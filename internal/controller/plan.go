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

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// 1インスタンス分の解決済み接続パラメータを保持
// managed/externalの分岐を平坦化し、ビルダー側で分岐不要にする
type plan struct {
	// Postgres
	dbManaged bool
	dbHost    string
	dbPort    int32
	dbName    string
	dbUser    string
	dbPassSel corev1.SecretKeySelector

	// Redis
	redisManaged bool
	redisHost    string
	redisPort    int32
	redisPassSel *corev1.SecretKeySelector

	// MeiliSearch/検索
	provider     misskeyv1alpha1.SearchProvider
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
}

// specを既定値適用の上でplanに平坦化
func resolve(m *misskeyv1alpha1.Misskey) plan {
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
		p.dbHost = nameDBService(m)
		p.dbPort = postgresPort
		p.dbName = stringOr(m.Spec.Postgres.Database, "misskey")
		p.dbUser = stringOr(m.Spec.Postgres.Owner, "misskey")
		// CNPGがapp secret <cluster>-appをusername/passwordキーで生成
		p.dbPassSel = corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: nameDBAppSecret(m)},
			Key:                  "password",
		}
	}

	// --- Redis ---
	if ext := m.Spec.Redis.External; ext != nil {
		p.redisManaged = false
		p.redisHost = ext.Host
		p.redisPort = int32OrDefault(ext.Port, redisPort)
		p.redisPassSel = ext.PasswordSecret
	} else {
		p.redisManaged = true
		p.redisHost = nameRedis(m)
		p.redisPort = redisPort
	}

	// --- Search ---
	p.provider = misskeyv1alpha1.SearchProvider(stringOr(string(m.Spec.Search.Provider), string(misskeyv1alpha1.SearchMeilisearch)))
	if p.provider == misskeyv1alpha1.SearchMeilisearch {
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
