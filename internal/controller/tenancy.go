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
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

const instanceLabel = "app.kubernetes.io/instance"

// reconcileTenancy: テナント隔離リソース
func (r *MisskeyReconciler) reconcileTenancy(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	return r.reconcileNetworkIsolation(ctx, m)
}

// reconcileNetworkIsolation: backend podへのingressをintra-instanceに限る
// 公開入口(proxy有効時proxy、無効時app)は開放し、ingress controllerから到達可能に保つ
func (r *MisskeyReconciler) reconcileNetworkIsolation(ctx context.Context, m *misskeyv1alpha1.Misskey) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-isolation", Namespace: m.Namespace}}
	if !boolOr(m.Spec.NetworkIsolation, true) {
		return r.deleteIfExists(ctx, np)
	}
	publicEntry := "proxy"
	if !boolOr(m.Spec.Proxy.Enabled, true) {
		publicEntry = roleApp
	}
	return r.apply(ctx, m, np, func() error {
		np.Labels = labelsFor(m, "isolation")
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{instanceLabel: m.Name},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "app.kubernetes.io/component",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{publicEntry},
			}},
		}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: m.Name}}},
			},
		}}
		return nil
	})
}

// deleteIfExists: 存在すれば削除(オプション無効化時のcleanup用)
func (r *MisskeyReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
