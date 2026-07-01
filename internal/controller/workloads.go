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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// reconcilePDB ensures a PodDisruptionBudget for a component so a node drain
// cannot take all replicas at once, which the zero-downtime rollout strategy
// otherwise implies but does not guarantee. maxUnavailable=1 still lets a
// single-replica component be drained.
func (r *MisskeyReconciler) reconcilePDB(ctx context.Context, m *misskeyv1alpha1.Misskey, component string) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-" + component, Namespace: m.Namespace}}
	return r.apply(ctx, m, pdb, func() error {
		pdb.Labels = labelsFor(m, component)
		mu := intstr.FromInt32(1)
		pdb.Spec.MaxUnavailable = &mu
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, component)}
		return nil
	})
}

// rollingZeroDowntime is the shared surge/unavailable policy: never drop below
// the desired count, add at most one extra pod.
func rollingZeroDowntime() appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: ptrIntStr(intstr.FromInt32(0)),
			MaxSurge:       ptrIntStr(intstr.FromInt32(1)),
		},
	}
}

func ptrIntStr(v intstr.IntOrString) *intstr.IntOrString { return &v }

// setDeployment fills a Deployment's fields idempotently for CreateOrUpdate.
// annotations are stamped on the pod template (e.g. the config checksum) so a
// config change triggers a rolling update.
func setDeployment(dep *appsv1.Deployment, m *misskeyv1alpha1.Misskey, component string, replicas *int32, pod corev1.PodSpec, annotations map[string]string) {
	dep.Labels = labelsFor(m, component)
	dep.Spec.Replicas = replicas
	dep.Spec.Strategy = rollingZeroDowntime()
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, component)}
	dep.Spec.Template.ObjectMeta.Labels = labelsFor(m, component)
	dep.Spec.Template.ObjectMeta.Annotations = annotations
	dep.Spec.Template.Spec = pod
}

// reconcileApp creates/updates the app Service and Deployment.
func (r *MisskeyReconciler) reconcileApp(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameApp(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, roleApp)
		svc.Spec.Selector = selectorFor(m, roleApp)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       misskeyPort,
			TargetPort: intstr.FromInt32(misskeyPort),
		}}
		return nil
	}); err != nil {
		return err
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameApp(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, dep, func() error {
		pod := buildMisskeyPodSpec(m, p, roleApp, m.Spec.App)
		setDeployment(dep, m, roleApp, replicasOr(m.Spec.App.Replicas, 1), pod, checksumAnnotation(renderDefaultYML(m, p)))
		return nil
	}); err != nil {
		return err
	}
	return r.reconcilePDB(ctx, m, roleApp)
}

// reconcileWorker creates/updates the worker Deployment.
func (r *MisskeyReconciler) reconcileWorker(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameWorker(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, dep, func() error {
		pod := buildMisskeyPodSpec(m, p, roleWorker, m.Spec.Worker)
		setDeployment(dep, m, roleWorker, replicasOr(m.Spec.Worker.Replicas, 1), pod, checksumAnnotation(renderDefaultYML(m, p)))
		return nil
	}); err != nil {
		return err
	}
	return r.reconcilePDB(ctx, m, roleWorker)
}
