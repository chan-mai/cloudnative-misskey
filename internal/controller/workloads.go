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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// コンポーネントのPodDisruptionBudgetを保証し、ノードdrainで全レプリカが一度に落ちないようにする
// zero-downtimeロールアウト戦略が暗に前提とするが保証しない点を担保。maxUnavailable=1なら単一レプリカのコンポーネントもdrain可能
func (r *MisskeyReconciler) reconcilePDB(ctx context.Context, m *misskeyv1beta1.Misskey, component string) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: m.Name + "-" + component, Namespace: m.Namespace}}
	return r.apply(ctx, m, pdb, func() error {
		pdb.Labels = labelsFor(m, component)
		mu := intstr.FromInt32(1)
		pdb.Spec.MaxUnavailable = &mu
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, component)}
		return nil
	})
}

// 共通のsurge/unavailableポリシー。desired数を下回らず、追加podは最大1つ
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

// CreateOrUpdate用にDeploymentのフィールドを冪等に埋める
// annotation(例: configチェックサム)はpodテンプレートにマージし、他ツールが付与したテンプレートannotationを潰さずconfig変更でローリング更新を起こす
// replicasがnilの時はReplicasを触らない(autoscaling有効時、HPA/KEDA管理値を保持)
func setDeployment(dep *appsv1.Deployment, m *misskeyv1beta1.Misskey, component string, replicas *int32, pod corev1.PodSpec, annotations map[string]string) {
	dep.Labels = labelsFor(m, component)
	if replicas != nil {
		dep.Spec.Replicas = replicas
	}
	dep.Spec.Strategy = rollingZeroDowntime()
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorFor(m, component)}
	dep.Spec.Template.Labels = labelsFor(m, component)
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		dep.Spec.Template.Annotations[k] = v
	}
	dep.Spec.Template.Spec = pod
}

// app Service(常時)。app Deployment未作成でもproxyのtarget先になる
func (r *MisskeyReconciler) reconcileAppService(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: nameApp(m), Namespace: m.Namespace}}
	return r.apply(ctx, m, svc, func() error {
		svc.Labels = labelsFor(m, roleApp)
		svc.Spec.Selector = selectorFor(m, roleApp)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       misskeyPort,
			TargetPort: intstr.FromInt32(misskeyPort),
		}}
		return nil
	})
}

// app Deployment+PDB+autoscaler。MigrationComplete後にのみ呼ぶ
func (r *MisskeyReconciler) reconcileApp(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	sc := appScaleConfig(m.Spec.App.Autoscaling)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameApp(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, dep, func() error {
		cs, err := r.misskeyChecksum(ctx, m, p)
		if err != nil {
			return err
		}
		pod := buildMisskeyPodSpec(m, p, roleApp, m.Spec.App.ComponentSpec)
		setDeployment(dep, m, roleApp, resumeReplicas(m.Spec.App.ComponentSpec, sc, dep), pod, cs)
		return nil
	}); err != nil {
		return err
	}
	if err := r.reconcilePDB(ctx, m, roleApp); err != nil {
		return err
	}
	return r.reconcileAutoscaler(ctx, m, roleApp, nameApp(m), sc, p)
}

// workerのDeployment+PDB+autoscaler
func (r *MisskeyReconciler) reconcileWorker(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	sc := workerScaleConfig(m.Spec.Worker.Autoscaling)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: nameWorker(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, dep, func() error {
		cs, err := r.misskeyChecksum(ctx, m, p)
		if err != nil {
			return err
		}
		pod := buildMisskeyPodSpec(m, p, roleWorker, m.Spec.Worker.ComponentSpec)
		setDeployment(dep, m, roleWorker, resumeReplicas(m.Spec.Worker.ComponentSpec, sc, dep), pod, cs)
		return nil
	}); err != nil {
		return err
	}
	if err := r.reconcilePDB(ctx, m, roleWorker); err != nil {
		return err
	}
	return r.reconcileAutoscaler(ctx, m, roleWorker, nameWorker(m), sc, p)
}

// staticReplicas: autoscaling有効ならnil(autoscalerがreplicas管理)、無効ならreplicasOr
func staticReplicas(comp misskeyv1beta1.ComponentSpec, a *scaleConfig) *int32 {
	if autoscalingEnabled(a) {
		return nil
	}
	return replicasOr(comp.Replicas, 1)
}

// resumeReplicas: apply mutate内でのreplicas決定
// autoscaling有効かつ既存Deploymentがreplicas=0(suspend解除直後)ならminReplicasで再点火
// HPAはspec.replicas=0のtargetをautoscaling無効として無視するため、kickなしではスタックする
func resumeReplicas(comp misskeyv1beta1.ComponentSpec, a *scaleConfig, dep *appsv1.Deployment) *int32 {
	if replicas := staticReplicas(comp, a); replicas != nil {
		return replicas
	}
	if dep.ResourceVersion == "" || dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		return nil
	}
	return replicasOr(a.MinReplicas, 1)
}

// suspendWorkloads: app/workerをreplicas 0に落としautoscalerを削除
// Deployment不在時は何も作らない(初回install前のsuspendはworkload未生成のまま)
func (r *MisskeyReconciler) suspendWorkloads(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	for _, name := range []string{nameApp(m), nameWorker(m)} {
		if err := r.deleteHPA(ctx, m, name); err != nil {
			return err
		}
		if err := r.deleteScaledObject(ctx, m, name); err != nil {
			return err
		}
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, dep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
			dep.Spec.Replicas = int32Ptr(0)
			if err := r.Update(ctx, dep); err != nil {
				return err
			}
			r.event(m, corev1.EventTypeNormal, "Suspended", "Suspend", "scaled %s to 0 replicas", name)
		}
	}
	return nil
}

// misskeyChecksum: app/worker podテンプレートのchecksum annotation
// config本文+参照SecretのresourceVersion。objectStorage(autoConfigure時)も含め、
// meta直書きはpub/sub非発火のため設定/カラム/資格情報変更でpodをrollし古いmeta cacheを畳む
func (r *MisskeyReconciler) misskeyChecksum(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (map[string]string, error) {
	versions, err := r.referencedSecretVersions(ctx, m, p)
	if err != nil {
		return nil, err
	}
	parts := append([]string{renderDefaultYML(m, p)}, versions...)
	if p.objAutoConfigure {
		if assigns, err := objectStorageAssignments(p); err == nil {
			sql := renderObjectStorageSQL(assigns)
			parts = append(parts, r.objectStorageHash(ctx, m, p, sql, assigns))
		}
	}
	return checksumAnnotation(parts...), nil
}
