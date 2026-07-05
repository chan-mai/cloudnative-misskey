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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

const misskeyFinalizer = "cloudnative-misskey.dev/finalizer"

var (
	secretGVK      = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	statefulSetGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
)

type retainTarget struct {
	gvk  schema.GroupVersionKind
	name string
}

// reconcileDelete: deletionPolicy=Retainならデータ資源をorphan化してからfinalizerを外す
func (r *MisskeyReconciler) reconcileDelete(ctx context.Context, m *misskeyv1alpha1.Misskey) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(m, misskeyFinalizer) {
		return ctrl.Result{}, nil
	}
	if m.Spec.DeletionPolicy == "Retain" {
		if err := r.retainData(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
		r.event(m, corev1.EventTypeNormal, "DataRetained", "Delete", "detached owner references from data resources per deletionPolicy=Retain")
	}
	controllerutil.RemoveFinalizer(m, misskeyFinalizer)
	return ctrl.Result{}, r.Update(ctx, m)
}

// retainData: CR削除でデータが消えないよう、stateful資源とkey secretのownerRefを外す
// 同名CR再作成でSSA/CreateOrUpdateが再adoptしデータ込みで復帰する
func (r *MisskeyReconciler) retainData(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	targets := []retainTarget{
		{cnpgClusterGVK, nameDB(m)},    // DB(削除するとCNPGがPVCごと消す最大の消失点)
		{statefulSetGVK, nameMeili(m)}, // meili STS(継続稼働)
		{secretGVK, nameMeili(m)},      // meili master key(消えると既存indexへアクセス不可)
		{secretGVK, nameSetup(m)},      // setup password
		{secretGVK, nameRedisAuthSecret(m)},
	}
	// redisは全suffixのstandalone STS / HA CR
	for _, suffix := range allRedisSuffixes() {
		name := nameRedisInstance(m, suffix)
		targets = append(targets,
			retainTarget{statefulSetGVK, name},
			retainTarget{redisReplicationGVK, name},
			retainTarget{redisSentinelGVK, name},
		)
	}
	for _, t := range targets {
		if err := r.orphan(ctx, m, t.gvk, t.name); err != nil {
			return err
		}
	}
	return nil
}

// orphan: 対象のownerReferencesから当該Misskey(UID一致)を除去してGC対象から外す
// 不在(NotFound)・CRD未導入(NoMatch)は無視
func (r *MisskeyReconciler) orphan(ctx context.Context, m *misskeyv1alpha1.Misskey, gvk schema.GroupVersionKind, name string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, u); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	refs := u.GetOwnerReferences()
	kept := make([]metav1.OwnerReference, 0, len(refs))
	changed := false
	for _, ref := range refs {
		if ref.UID == m.UID {
			changed = true
			continue
		}
		kept = append(kept, ref)
	}
	if !changed {
		return nil
	}
	u.SetOwnerReferences(kept)
	return r.Update(ctx, u)
}
