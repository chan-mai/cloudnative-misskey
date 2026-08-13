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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

func deletingMisskey(policy string) *misskeyv1beta1.Misskey {
	m := newMisskey()
	m.UID = "uid-123"
	m.Spec.DeletionPolicy = policy
	m.Finalizers = []string{misskeyFinalizer}
	now := metav1.Now()
	m.DeletionTimestamp = &now
	return m
}

// ownedSecret: 当該Misskeyがcontroller ownerのSecret
func ownedSecret(m *misskeyv1beta1.Misskey, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: m.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "cloudnative-misskey.dev/v1beta1", Kind: "Misskey",
				Name: m.Name, UID: m.UID, Controller: boolPtr(true),
			}},
		},
	}
}

func deletionScheme() *runtime.Scheme {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = misskeyv1beta1.AddToScheme(sch)
	// unstructuredでGetする外部CRDをfake schemeへ登録(未存在時NotFoundになるように)
	for _, gvk := range []schema.GroupVersionKind{cnpgClusterGVK, redisReplicationGVK, redisSentinelGVK} {
		sch.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	return sch
}

func TestReconcileDeleteRetainOrphans(t *testing.T) {
	m := deletingMisskey("Retain")
	secrets := []client.Object{
		ownedSecret(m, nameMeili(m)),
		ownedSecret(m, nameSensitiveDetector(m)),
	}
	cl := fake.NewClientBuilder().WithScheme(deletionScheme()).WithObjects(append([]client.Object{m}, secrets...)...).Build()
	r := &MisskeyReconciler{Client: cl, Scheme: cl.Scheme()}

	if _, err := r.reconcileDelete(context.Background(), m); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}
	for _, name := range []string{nameMeili(m), nameSensitiveDetector(m)} {
		got := &corev1.Secret{}
		if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: m.Namespace}, got); err != nil {
			t.Fatalf("secret %s deleted despite Retain: %v", name, err)
		}
		if len(got.OwnerReferences) != 0 {
			t.Errorf("secret %s ownerRef not orphaned: %v", name, got.OwnerReferences)
		}
	}
}

func TestSensitiveDetectorSecretGeneratedCustomTransition(t *testing.T) {
	ctx := context.Background()
	m := newMisskey()
	m.UID = "uid-sensitive"
	m.Spec.SensitiveDetector = &misskeyv1beta1.SensitiveDetectorSpec{Mode: "all"}
	cl := fake.NewClientBuilder().WithScheme(deletionScheme()).Build()
	r := &MisskeyReconciler{Client: cl, Scheme: cl.Scheme()}

	if err := r.reconcileSensitiveDetectorSecret(ctx, m); err != nil {
		t.Fatalf("generate detector Secret: %v", err)
	}
	key := types.NamespacedName{Name: nameSensitiveDetector(m), Namespace: m.Namespace}
	generated := &corev1.Secret{}
	if err := cl.Get(ctx, key, generated); err != nil {
		t.Fatalf("get generated detector Secret: %v", err)
	}
	oldAPIKey := string(generated.Data[sensitiveDetectorAPIKeyID])

	m.Spec.SensitiveDetector.APIKeySecret = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "custom-detector-key"},
		Key:                  "token",
	}
	if err := cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-detector-key", Namespace: m.Namespace},
		Data:       map[string][]byte{"token": []byte("custom-key")},
	}); err != nil {
		t.Fatalf("create custom detector Secret: %v", err)
	}
	p := resolve(m)
	if err := r.finalizeSensitiveDetectorCleanup(ctx, m, p, true, nil); err != nil {
		t.Fatalf("check pending custom detector Secret transition: %v", err)
	}
	if err := cl.Get(ctx, key, &corev1.Secret{}); err != nil {
		t.Fatalf("generated detector Secret removed before rollout convergence: %v", err)
	}
	checksum, err := r.misskeyChecksum(ctx, m, p)
	if err != nil {
		t.Fatalf("calculate workload checksum: %v", err)
	}
	for _, name := range []string{nameApp(m), nameWorker(m)} {
		dep := convergedDeployment(name, m.Namespace)
		dep.Spec.Template.Annotations = checksum
		if err := cl.Create(ctx, dep); err != nil {
			t.Fatalf("create %s Deployment: %v", name, err)
		}
	}
	if err := cl.Create(ctx, convergedDeployment(nameSensitiveDetector(m), m.Namespace)); err != nil {
		t.Fatalf("create detector Deployment: %v", err)
	}
	if err := r.finalizeSensitiveDetectorCleanup(ctx, m, p, true, nil); err != nil {
		t.Fatalf("finalize custom detector Secret transition: %v", err)
	}
	if err := cl.Get(ctx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("generated detector Secret remains after custom selection: %v", err)
	}

	m.Spec.SensitiveDetector.APIKeySecret = nil
	if err := r.reconcileSensitiveDetectorSecret(ctx, m); err != nil {
		t.Fatalf("switch to generated detector Secret: %v", err)
	}
	regenerated := &corev1.Secret{}
	if err := cl.Get(ctx, key, regenerated); err != nil {
		t.Fatalf("get regenerated detector Secret: %v", err)
	}
	if got := string(regenerated.Data[sensitiveDetectorAPIKeyID]); got == "" || got == oldAPIKey {
		t.Errorf("regenerated API key=%q, want a new non-empty value", got)
	}
}

func convergedDeployment(name, namespace string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
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
		t.Errorf("Delete policy must not remove ownerRef: %v", got.OwnerReferences)
	}
}
