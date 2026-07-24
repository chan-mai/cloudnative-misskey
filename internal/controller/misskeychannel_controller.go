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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// MisskeyChannelReconciler: fleet imageチャンネルのロールアウト状態を管理
// spec.imageの変更検知でロールアウトを開始し、追従状況をstatusへ集計する
// 各インスタンスへの実際の反映はMisskeyReconcilerがchannelImageForで解決する
type MisskeyChannelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Digests: trackImageDigest時のtag→digest解決(Misskey controllerと共有)
	Digests *DigestResolver
}

// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeychannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeychannels/status,verbs=get;update;patch

func (r *MisskeyChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ch := &misskeyv1beta1.MisskeyChannel{}
	if err := r.Get(ctx, req.NamespacedName, ch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 配信対象imageを決定。trackImageDigest時はtag→digestでpinし、同一タグの
	// 中身変更もimage変更として段階ロールアウトに乗せる
	target := ch.Spec.Image
	var resolveErr error
	if ch.Spec.TrackImageDigest {
		if r.Digests == nil {
			return ctrl.Result{}, fmt.Errorf("trackImageDigest requested but no digest resolver configured")
		}
		// Channelはcluster-scopedでpull secret非依存のためanonymous(keyID空)で解決
		if pinned, err := r.Digests.Pinned(ctx, ch.Spec.Image, nil, ""); err == nil {
			target = pinned
		} else {
			// 解決不能時は現行statusを維持しrollout状態を壊さない。後段で短いrequeue
			resolveErr = err
			target = ch.Status.Image
		}
	}

	// 参照インスタンスと配信対象imageへの追従数を集計
	var list misskeyv1beta1.MisskeyList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	var instances, updated int32
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.ImageFrom == nil || m.Spec.ImageFrom.Channel != ch.Name {
			continue
		}
		instances++
		if target != "" && m.Status.Image == target {
			updated++
		}
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &misskeyv1beta1.MisskeyChannel{}
		if err := r.Get(ctx, req.NamespacedName, cur); err != nil {
			return err
		}
		// image変更検知でロールアウト開始。初回(previous無し)は即時全量
		if target != "" && target != cur.Status.Image {
			if cur.Status.Image != "" {
				cur.Status.PreviousImage = cur.Status.Image
				cur.Status.ImageChangedAt = metav1.Now()
			}
			cur.Status.Image = target
		}
		cur.Status.Instances = instances
		cur.Status.UpdatedInstances = updated
		cur.Status.ObservedGeneration = cur.Generation
		return r.Status().Update(ctx, cur)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if resolveErr != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// ロールアウト進行中はinterval周期、digest追従時はTTL周期でrequeue
	var requeue time.Duration
	if ch.Spec.Rollout != nil && updated < instances {
		requeue = ch.Spec.Rollout.Interval.Duration
		if requeue <= 0 {
			requeue = time.Hour
		}
	}
	if ch.Spec.TrackImageDigest && (requeue == 0 || digestResolveTTL < requeue) {
		requeue = digestResolveTTL
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// channelOfMisskey: Misskeyの変化を参照先Channelのreconcileへ写像(追従カウンタ更新)
func (r *MisskeyChannelReconciler) channelOfMisskey(ctx context.Context, obj client.Object) []reconcile.Request {
	m, ok := obj.(*misskeyv1beta1.Misskey)
	if !ok || m.Spec.ImageFrom == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: m.Spec.ImageFrom.Channel}}}
}

func (r *MisskeyChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&misskeyv1beta1.MisskeyChannel{}).
		Watches(&misskeyv1beta1.Misskey{}, handler.EnqueueRequestsFromMapFunc(r.channelOfMisskey)).
		Complete(r)
}
