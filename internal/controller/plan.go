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
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// plan holds the resolved connection parameters for one instance. It flattens
// the "managed vs external" branches so the builders don't have to.
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

	// MeiliSearch / search
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

	// Setup password
	setupEnabled bool
	setupManaged bool // operator generates the Secret
	setupSel     corev1.SecretKeySelector
}

// resolve flattens the spec into a plan, applying defaults.
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
		// CNPG generates the app secret <cluster>-app with keys username/password.
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

// int32OrDefault returns v or def when v is zero.
func int32OrDefault(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// hostFromURL extracts the host component from a URL, tolerating bad input.
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Fall back to a best-effort strip of scheme and path.
		s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
		if i := strings.IndexAny(s, "/:"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return u.Hostname()
}

// sanitizeIndex converts a host into a MeiliSearch-safe index name (alphanumeric,
// hyphen, underscore only).
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
