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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

func deletingMisskey(policy string) *misskeyv1alpha1.Misskey {
	m := newMisskey()
	m.UID = "uid-123"
	m.Spec.DeletionPolicy = policy
	m.Finalizers = []string{misskeyFinalizer}
	now := metav1.Now()
	m.DeletionTimestamp = &now
	return m
}

// ownedSecret: 当該Misskeyがcontroller ownerのSecret
func ownedSecret(m *misskeyv1alpha1.Misskey, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: m.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "cloudnative-misskey.dev/v1alpha1", Kind: "Misskey",
				Name: m.Name, UID: m.UID, Controller: boolPtr(true),
			}},
		},
	}
}

func deletionScheme() *runtime.Scheme {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = misskeyv1alpha1.AddToScheme(sch)
	// unstructuredでGetする外部CRDをfake schemeへ登録(未存在時NotFoundになるように)
	for _, gvk := range []schema.GroupVersionKind{cnpgClusterGVK, redisReplicationGVK, redisSentinelGVK} {
		sch.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	return sch
}

func TestReconcileDeleteRetainOrphans(t *testing.T) {
	m := deletingMisskey("Retain")
	sec := ownedSecret(m, nameMeili(m))
	cl := fake.NewClientBuilder().WithScheme(deletionScheme()).WithObjects(m, sec).Build()
	r := &MisskeyReconciler{Client: cl, Scheme: cl.Scheme()}

	if _, err := r.reconcileDelete(context.Background(), m); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}
	got := &corev1.Secret{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: nameMeili(m), Namespace: m.Namespace}, got); err != nil {
		t.Fatalf("retain時にsecretが消えた: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("ownerRefがorphan化されていない: %v", got.OwnerReferences)
	}
}

func TestReconcileDeleteDeleteKeepsOwnerRef(t *testing.T) {
	m := deletingMisskey("Delete")
	sec := ownedSecret(m, nameMeili(m))
	cl := fake.NewClientBuilder().WithScheme(deletionScheme()).WithObjects(m, sec).Build()
	r := &MisskeyReconciler{Client: cl, Scheme: cl.Scheme()}

	if _, err := r.reconcileDelete(context.Background(), m); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}
	// Delete方針ではorphanせず、GC(実クラスタ)に委ねる=ownerRefは残る
	got := &corev1.Secret{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: nameMeili(m), Namespace: m.Namespace}, got); err != nil {
		t.Fatalf("secret get: %v", err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Errorf("Delete方針でownerRefを外してはいけない: %v", got.OwnerReferences)
	}
}
