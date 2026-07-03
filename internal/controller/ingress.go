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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// クラス固有の既定値とユーザのannotationをマージ(ユーザ優先)
// nginxではproxy-body-sizeを引き上げる。既定1MBだとメディアアップロードが弾かれるため
func ingressAnnotations(m *misskeyv1alpha1.Misskey, className string) map[string]string {
	out := map[string]string{}
	if strings.Contains(className, "nginx") {
		out["nginx.ingress.kubernetes.io/proxy-body-size"] = "0"
	}
	for k, v := range m.Spec.Ingress.Annotations {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// 公開ホストをproxy(プロキシ無効時はapp直)へルーティングするIngressを作成/更新
func (r *MisskeyReconciler) reconcileIngress(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	// 有効時はproxy、そうでなければapp直を対象
	backendName := nameProxy(m)
	var backendPort int32 = 80
	if !boolOr(m.Spec.Proxy.Enabled, true) {
		backendName = nameApp(m)
		backendPort = misskeyPort
	}

	pathType := networkingv1.PathTypePrefix
	className := stringOr(m.Spec.Ingress.ClassName, "nginx")

	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace}}
	return r.apply(ctx, m, ing, func() error {
		ing.Labels = labelsFor(m, "ingress")
		ing.Annotations = ingressAnnotations(m, className)
		ing.Spec.IngressClassName = &className
		ing.Spec.Rules = []networkingv1.IngressRule{
			{
				Host: p.ingressHost,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: backendName,
										Port: networkingv1.ServiceBackendPort{Number: backendPort},
									},
								},
							},
						},
					},
				},
			},
		}
		if m.Spec.Ingress.TLSSecretName != "" {
			ing.Spec.TLS = []networkingv1.IngressTLS{
				{Hosts: []string{p.ingressHost}, SecretName: m.Spec.Ingress.TLSSecretName},
			}
		} else {
			ing.Spec.TLS = nil
		}
		return nil
	})
}
