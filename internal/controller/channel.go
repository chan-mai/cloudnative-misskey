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
	"time"

	"k8s.io/apimachinery/pkg/types"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
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
func channelImageFor(ch *misskeyv1alpha1.MisskeyChannel, bucket uint32, now time.Time) string {
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

// resolveImage: imageFrom時にChannelからimageを解決しm.Spec.Imageへin-memory代入する
// 代入はこのreconcileの計算にのみ使い、絶対にpersistしない(specへのUpdateはfinalizer付与のみで
// resolveImageより前に完了している)。以降のnameMigrate/podSpec/checksumは無変更で解決値を使う
func (r *MisskeyReconciler) resolveImage(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	if m.Spec.ImageFrom == nil {
		return nil
	}
	ch := &misskeyv1alpha1.MisskeyChannel{}
	if err := r.Get(ctx, types.NamespacedName{Name: m.Spec.ImageFrom.Channel}, ch); err != nil {
		return fmt.Errorf("resolve imageFrom.channel %q: %w", m.Spec.ImageFrom.Channel, err)
	}
	m.Spec.Image = channelImageFor(ch, channelBucket(m.Namespace, m.Name), time.Now())
	return nil
}
