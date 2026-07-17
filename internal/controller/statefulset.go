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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// applyStatefulSet: STS版のapply。in-place更新できないvolumeClaimTemplatesの変更
// (ストレージサイズ/StorageClass)はorphan削除→再作成で反映する。pod/PVCは残り
// 再作成後のSTSがselectorで再採用するため無停止・データ保持。
// サイズ増は既存PVCへも反映する(StorageClassがallowVolumeExpansionの場合のみ受理され、
// 不可なら警告Eventを出してテンプレート更新のみ行う)。PVCは縮小不可のため据え置き
func (r *MisskeyReconciler) applyStatefulSet(ctx context.Context, m *misskeyv1beta1.Misskey, sts *appsv1.StatefulSet, mutate func() error) error {
	if err := mutate(); err != nil {
		return err
	}
	desired := sts.Spec.DeepCopy().VolumeClaimTemplates

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(sts), existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		// orphan削除のGC処理(finalizer除去)待ちの間は名前を再利用できない。エラーで返しbackoff再試行に委ねる
		if existing.DeletionTimestamp != nil {
			return fmt.Errorf("statefulset %s is terminating, waiting to recreate", sts.Name)
		}
		if volumeClaimTemplatesChanged(existing.Spec.VolumeClaimTemplates, desired) {
			r.expandStatefulSetPVCs(ctx, m, existing, desired)
			orphan := metav1.DeletePropagationOrphan
			if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &orphan}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			r.event(m, corev1.EventTypeNormal, "StatefulSetRecreated", "ApplyStatefulSet",
				"recreating StatefulSet %s to apply volume claim changes (pods and PVCs are retained)", sts.Name)
			// GCの孤児化は通常数秒で終わるため短時間だけ待ち、間に合わなければ再試行に委ねる
			if pollErr := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				getErr := r.Get(ctx, client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{})
				return apierrors.IsNotFound(getErr), nil
			}); pollErr != nil {
				return fmt.Errorf("statefulset %s is terminating, waiting to recreate", sts.Name)
			}
		}
	}
	return r.apply(ctx, m, sts, mutate)
}

// volumeClaimTemplatesChanged: operatorが管理するフィールド(名前/サイズ/StorageClass)の差分のみ見る
// (accessModes等のサーバ既定値で誤検知して再作成ループしないため)
func volumeClaimTemplatesChanged(current, desired []corev1.PersistentVolumeClaim) bool {
	if len(current) != len(desired) {
		return true
	}
	for i := range desired {
		c, d := current[i], desired[i]
		if c.Name != d.Name {
			return true
		}
		if ptr.Deref(c.Spec.StorageClassName, "") != ptr.Deref(d.Spec.StorageClassName, "") {
			return true
		}
		cq := c.Spec.Resources.Requests[corev1.ResourceStorage]
		dq := d.Spec.Resources.Requests[corev1.ResourceStorage]
		if cq.Cmp(dq) != 0 {
			return true
		}
	}
	return false
}

// expandStatefulSetPVCs: テンプレートのサイズ増を既存PVC(<claim>-<sts>-<ordinal>)へ反映する
// 拡張を受理できない場合(StorageClassがallowVolumeExpansion無効等)はreconcileを止めず警告Eventに留める
func (r *MisskeyReconciler) expandStatefulSetPVCs(ctx context.Context, m *misskeyv1beta1.Misskey, sts *appsv1.StatefulSet, desired []corev1.PersistentVolumeClaim) {
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	for _, tpl := range desired {
		want, ok := tpl.Spec.Resources.Requests[corev1.ResourceStorage]
		if !ok {
			continue
		}
		for i := int32(0); i < replicas; i++ {
			pvc := &corev1.PersistentVolumeClaim{}
			key := types.NamespacedName{Name: fmt.Sprintf("%s-%s-%d", tpl.Name, sts.Name, i), Namespace: sts.Namespace}
			if err := r.Get(ctx, key, pvc); err != nil {
				continue
			}
			got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			if want.Cmp(got) <= 0 {
				continue
			}
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
			if err := r.Update(ctx, pvc); err != nil {
				r.event(m, corev1.EventTypeWarning, "PVCExpandFailed", "ApplyStatefulSet",
					"failed to expand PVC %s to %s: %v", key.Name, want.String(), err)
			}
		}
	}
}
