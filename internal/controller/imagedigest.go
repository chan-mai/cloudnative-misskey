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
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// digestResolveTTL: 同一imageのレジストリ再問い合わせ間隔
// mutableタグの変更検知はこのTTL+reconcile周期の遅れで反映される
const digestResolveTTL = 5 * time.Minute

// digestResolveTimeout: レジストリ1回あたりの問い合わせ上限
const digestResolveTimeout = 10 * time.Second

// maxCacheEntries: digest cacheのエントリ上限。ユニークtag大量作成によるメモリ肥大(DoS)を防ぐ
const maxCacheEntries = 1024

// cacheKey: image + 認証コンテキスト識別子。keyIDでテナント跨ぎの解決結果共有を防ぐ
// (テナントAのpull secretで解決したprivate digestがテナントBへ返るのを回避, 空=anonymous共有)
func cacheKey(image, keyID string) string {
	return image + "\x00" + keyID
}

// DigestResolver: image参照をレジストリで解決しimage@digestへpinする(TTL cache付き)
// headFuncはテスト差し替え用。両controllerで1インスタンスを共有する
type DigestResolver struct {
	mu       sync.Mutex
	cache    map[string]digestEntry
	headFunc func(ctx context.Context, image string, keychain authn.Keychain) (string, error)
}

type digestEntry struct {
	digest     string
	resolvedAt time.Time
}

func NewDigestResolver() *DigestResolver {
	return &DigestResolver{
		cache:    map[string]digestEntry{},
		headFunc: registryHead,
	}
}

// registryHead: レジストリのmanifest HEADでtagのdigestを取得
func registryHead(ctx context.Context, image string, keychain authn.Keychain) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", fmt.Errorf("parse image %q: %w", image, err)
	}
	ctx, cancel := context.WithTimeout(ctx, digestResolveTimeout)
	defer cancel()
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain))
	if err != nil {
		return "", fmt.Errorf("resolve digest of %q: %w", image, err)
	}
	return desc.Digest.String(), nil
}

// Pinned: imageをimage@digestへ解決する。digest指定済みならそのまま
// keyIDはpull secret由来の認証コンテキスト識別子でcacheをテナント分離する
// 失敗時はTTL切れでもcacheがあればstaleを返す(pin↔非pinのflapでpodを無駄にrollさせない)
func (r *DigestResolver) Pinned(ctx context.Context, image string, keychain authn.Keychain, keyID string) (string, error) {
	if strings.Contains(image, "@") {
		return image, nil
	}
	key := cacheKey(image, keyID)
	r.mu.Lock()
	entry, ok := r.cache[key]
	r.mu.Unlock()
	if ok && time.Since(entry.resolvedAt) < digestResolveTTL {
		return image + "@" + entry.digest, nil
	}
	digest, err := r.headFunc(ctx, image, keychain)
	if err != nil {
		if ok {
			return image + "@" + entry.digest, nil
		}
		return "", err
	}
	r.mu.Lock()
	r.evictLocked()
	r.cache[key] = digestEntry{digest: digest, resolvedAt: time.Now()}
	r.mu.Unlock()
	return image + "@" + digest, nil
}

// evictLocked: 上限到達時に失効エントリを一掃し、なお超過なら最古を1件落とす。r.mu保持前提
func (r *DigestResolver) evictLocked() {
	if len(r.cache) < maxCacheEntries {
		return
	}
	for k, e := range r.cache {
		if time.Since(e.resolvedAt) >= digestResolveTTL {
			delete(r.cache, k)
		}
	}
	if len(r.cache) < maxCacheEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	first := true
	for k, e := range r.cache {
		if first || e.resolvedAt.Before(oldest) {
			oldest, oldestKey, first = e.resolvedAt, k, false
		}
	}
	delete(r.cache, oldestKey)
}
