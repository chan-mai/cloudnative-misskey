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

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// クラス固有の既定値とユーザのannotationをマージ
// ユーザ指定を先に適用し(危険キーは除外)、operator管理キー(cert-manager/proxy-body-size)を
// 後段で上書きして後勝ちにする。nginxではproxy-body-sizeを引き上げる(既定1MBだとメディア弾き)
func ingressAnnotations(m *misskeyv1beta1.Misskey, className string) map[string]string {
	out := map[string]string{}
	for k, v := range m.Spec.Ingress.Annotations {
		if isDangerousIngressAnnotation(k) {
			continue
		}
		out[k] = v
	}
	if strings.Contains(className, "nginx") {
		out["nginx.ingress.kubernetes.io/proxy-body-size"] = "0"
	}
	if ref := m.Spec.Ingress.IssuerRef; ref != nil {
		key := "cert-manager.io/cluster-issuer"
		if ref.Kind == "Issuer" {
			key = "cert-manager.io/issuer"
		}
		out[key] = ref.Name
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isDangerousIngressAnnotation: Ingress controllerで特権昇格・任意設定注入に使われうるannotationを判定
// snippet系(任意nginx.conf注入, CVE-2021-25742類)とauth/redirect系(認証迂回・オープンリダイレクト)を遮断
func isDangerousIngressAnnotation(key string) bool {
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "-snippet") {
		return true
	}
	for _, bad := range []string{
		"nginx.ingress.kubernetes.io/auth-url",
		"nginx.ingress.kubernetes.io/auth-tls-",
		"nginx.ingress.kubernetes.io/server-alias",
		"nginx.ingress.kubernetes.io/permanent-redirect",
		"nginx.ingress.kubernetes.io/lua-",
		"nginx.ingress.kubernetes.io/mirror-",
	} {
		if strings.HasPrefix(k, bad) {
			return true
		}
	}
	return false
}

// 公開ホストをproxy(プロキシ無効時はapp直)へルーティングするIngressを作成/更新
// ingress無効化時は既存Ingressを掃除
func (r *MisskeyReconciler) reconcileIngress(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) error {
	if !boolOr(m.Spec.Ingress.Enabled, true) {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: m.Name, Namespace: m.Namespace}}
		return r.deleteIfExists(ctx, ing)
	}

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
		// issuerRef時はTLS blockを常設し、cert-managerに<name>-tlsへ証明書を払い出させる
		if m.Spec.Ingress.TLSSecretName != "" || m.Spec.Ingress.IssuerRef != nil {
			ing.Spec.TLS = []networkingv1.IngressTLS{
				{Hosts: []string{p.ingressHost}, SecretName: stringOr(m.Spec.Ingress.TLSSecretName, m.Name+"-tls")},
			}
		} else {
			ing.Spec.TLS = nil
		}
		return nil
	})
}
