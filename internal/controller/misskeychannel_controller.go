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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
)

// MisskeyChannelReconciler: fleet imageチャンネルのロールアウト状態を管理
// spec.imageの変更検知でロールアウトを開始し、追従状況をstatusへ集計する
// 各インスタンスへの実際の反映はMisskeyReconcilerがchannelImageForで解決する
type MisskeyChannelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeychannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloudnative-misskey.dev,resources=misskeychannels/status,verbs=get;update;patch

func (r *MisskeyChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ch := &misskeyv1alpha1.MisskeyChannel{}
	if err := r.Get(ctx, req.NamespacedName, ch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 参照インスタンスと現行imageへの追従数を集計
	var list misskeyv1alpha1.MisskeyList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	target := ch.Spec.Image
	var instances, updated int32
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.ImageFrom == nil || m.Spec.ImageFrom.Channel != ch.Name {
			continue
		}
		instances++
		if m.Status.Image == target {
			updated++
		}
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &misskeyv1alpha1.MisskeyChannel{}
		if err := r.Get(ctx, req.NamespacedName, cur); err != nil {
			return err
		}
		// image変更検知でロールアウト開始。初回(previous無し)は即時全量
		if cur.Spec.Image != cur.Status.Image {
			if cur.Status.Image != "" {
				cur.Status.PreviousImage = cur.Status.Image
				cur.Status.ImageChangedAt = metav1.Now()
			}
			cur.Status.Image = cur.Spec.Image
		}
		cur.Status.Instances = instances
		cur.Status.UpdatedInstances = updated
		cur.Status.ObservedGeneration = cur.Generation
		return r.Status().Update(ctx, cur)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// ロールアウト進行中はinterval周期でカウンタを追従
	if ch.Spec.Rollout != nil && updated < instances {
		interval := ch.Spec.Rollout.Interval.Duration
		if interval <= 0 {
			interval = time.Hour
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	return ctrl.Result{}, nil
}

// channelOfMisskey: Misskeyの変化を参照先Channelのreconcileへ写像(追従カウンタ更新)
func (r *MisskeyChannelReconciler) channelOfMisskey(ctx context.Context, obj client.Object) []reconcile.Request {
	m, ok := obj.(*misskeyv1alpha1.Misskey)
	if !ok || m.Spec.ImageFrom == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: m.Spec.ImageFrom.Channel}}}
}

func (r *MisskeyChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&misskeyv1alpha1.MisskeyChannel{}).
		Watches(&misskeyv1alpha1.Misskey{}, handler.EnqueueRequestsFromMapFunc(r.channelOfMisskey)).
		Complete(r)
}
