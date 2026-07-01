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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1alpha1 "github.com/chan-mai/cloud-native-misskey/api/v1alpha1"
)

// ingressAnnotations merges a class-specific default with the user's annotations
// (user wins). For nginx it raises proxy-body-size, whose 1MB default would
// otherwise reject media uploads.
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

// reconcileIngress creates/updates the Ingress routing the public host to the
// proxy (or directly to the app when the proxy is disabled).
func (r *MisskeyReconciler) reconcileIngress(ctx context.Context, m *misskeyv1alpha1.Misskey, p plan) error {
	// Target the proxy when enabled, else the app directly.
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
