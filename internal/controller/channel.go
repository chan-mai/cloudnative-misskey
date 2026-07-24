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
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	kauth "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// channelBucket: インスタンスを0-99の安定バケットへ写像(段階ロールアウトの割当)
func channelBucket(namespace, name string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(namespace + "/" + name))
	return h.Sum32() % 100
}

// channelImageFor: バケットと経過時間からこのインスタンスが従うimageを決定
// statusのみを読む(channel controllerの観測とMisskey側の解決を一貫させる)
// 変更直後に第1バッチが切替わり、interval毎にbatchPercentずつ広がる
func channelImageFor(ch *misskeyv1beta1.MisskeyChannel, bucket uint32, now time.Time) string {
	img := ch.Status.Image
	if img == "" {
		// channel controller未反映のbootstrap
		return ch.Spec.Image
	}
	prev := ch.Status.PreviousImage
	if prev == "" || prev == img || ch.Spec.Rollout == nil || ch.Status.ImageChangedAt.IsZero() {
		return img
	}
	interval := ch.Spec.Rollout.Interval.Duration
	if interval <= 0 {
		interval = time.Hour
	}
	elapsed := now.Sub(ch.Status.ImageChangedAt.Time)
	if elapsed < 0 {
		elapsed = 0
	}
	batches := int64(elapsed/interval) + 1
	threshold := int64(int32OrDefault(ch.Spec.Rollout.BatchPercent, 20)) * batches
	if int64(bucket) < threshold {
		return img
	}
	return prev
}

// resolveImage: imageFromのChannel解決とtrackImageDigestのdigest pinをm.Spec.Imageへ
// in-memory代入する。代入はこのreconcileの計算にのみ使い、絶対にpersistしない(specへの
// Updateはfinalizer付与のみでresolveImageより前に完了している)
// 以降のnameMigrate/podSpec/checksumは無変更で解決値を使う
func (r *MisskeyReconciler) resolveImage(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	if m.Spec.ImageFrom != nil {
		ch := &misskeyv1beta1.MisskeyChannel{}
		if err := r.Get(ctx, types.NamespacedName{Name: m.Spec.ImageFrom.Channel}, ch); err != nil {
			return fmt.Errorf("resolve imageFrom.channel %q: %w", m.Spec.ImageFrom.Channel, err)
		}
		m.Spec.Image = channelImageFor(ch, channelBucket(m.Namespace, m.Name), time.Now())
	}
	if !m.Spec.TrackImageDigest || strings.Contains(m.Spec.Image, "@") {
		return nil
	}
	if r.Digests == nil {
		return fmt.Errorf("trackImageDigest requested but no digest resolver configured")
	}
	keychain, err := r.pullSecretKeychain(ctx, m)
	if err != nil {
		return err
	}
	pinned, err := r.Digests.Pinned(ctx, m.Spec.Image, keychain, pullSecretKeyID(m))
	if err != nil {
		// レジストリ不達かつcache無し(operator再起動直後等)は直前にstatusへ出したpinを継続し、
		// bare tagへのflapによる無用なrollを避ける。それも無い初回のみエラー
		if strings.HasPrefix(m.Status.Image, m.Spec.Image+"@") {
			m.Spec.Image = m.Status.Image
			return nil
		}
		return fmt.Errorf("track image digest: %w", err)
	}
	m.Spec.Image = pinned
	return nil
}

// pullSecretKeyID: digest cacheの認証コンテキスト識別子
// pull secretのnamespace+名前集合でテナント跨ぎのprivate digest共有を防ぐ(無指定は空=公開image共有)
func pullSecretKeyID(m *misskeyv1beta1.Misskey) string {
	if len(m.Spec.ImagePullSecrets) == 0 {
		return ""
	}
	names := make([]string, 0, len(m.Spec.ImagePullSecrets))
	for _, ref := range m.Spec.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	return m.Namespace + "\x00" + strings.Join(names, ",")
}

// pullSecretKeychain: imagePullSecretsからレジストリ認証keychainを構築(未指定はnil=anonymous)
func (r *MisskeyReconciler) pullSecretKeychain(ctx context.Context, m *misskeyv1beta1.Misskey) (authn.Keychain, error) {
	if len(m.Spec.ImagePullSecrets) == 0 {
		return nil, nil
	}
	secrets := make([]corev1.Secret, 0, len(m.Spec.ImagePullSecrets))
	for _, ref := range m.Spec.ImagePullSecrets {
		s := corev1.Secret{}
		// SecretはClientキャッシュ無効(DisableFor)のためAPI直読
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: m.Namespace}, &s); err != nil {
			return nil, fmt.Errorf("imagePullSecret %q: %w", ref.Name, err)
		}
		secrets = append(secrets, s)
	}
	return kauth.NewFromPullSecrets(ctx, secrets)
}
