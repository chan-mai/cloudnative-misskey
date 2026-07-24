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

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
	"github.com/chan-mai/cloudnative-misskey/internal/controller"
	webhookv1beta1 "github.com/chan-mai/cloudnative-misskey/internal/webhook/v1beta1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(misskeyv1beta1.AddToScheme(scheme))
}

// splitCSV: カンマ区切りをtrim+空要素除去でスライス化
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var driftResyncInterval time.Duration
	var watchNamespaces string
	var allowedImageRegistries string
	var allowedClusterIssuers string
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces to watch. Empty watches all namespaces (cluster-scoped). "+
			"When set, namespaced resources (Misskey and its children) can be granted via per-namespace "+
			"RoleBindings, but the cluster-scoped MisskeyChannel CRD still requires a small ClusterRole "+
			"for get;list;watch on misskeychannels.")
	flag.StringVar(&allowedImageRegistries, "allowed-image-registries", "",
		"Comma-separated allowed image reference prefixes. Empty allows any. When set, the webhook "+
			"rejects spec.image/objectStorage.image/MisskeyChannel.image outside the list.")
	flag.StringVar(&allowedClusterIssuers, "allowed-cluster-issuers", "",
		"Comma-separated allowed cert-manager ClusterIssuer names for spec.ingress.issuerRef. Empty allows any.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"Serve the metrics endpoint over HTTPS with authn/authz. Disable only for trusted local use.")
	flag.DurationVar(&driftResyncInterval, "drift-resync-interval", 3*time.Minute,
		"Interval to re-reconcile each Misskey and re-apply externally-managed CRDs "+
			"(CNPG/redis-operator/KEDA) to correct drift. 0 uses the built-in default.")
	// 既定は本番ロギング。--zap-develで詳細な開発ログ
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// webhook/metricsサーバのHTTP/2脆弱性を緩和
	disableHTTP2 := func(c *tls.Config) {
		c.NextProtos = []string{"http/1.1"}
	}

	metricsOpts := server.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
	}
	if secureMetrics {
		// metricsエンドポイントをKubernetesのauthn/authzで保護
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// namespaced運用: --watch-namespaces指定時はinformer cacheを限定し、ClusterRoleでなく
	// namespace別RoleBindingで縛れるようにする(マルチテナントのブラスト半径縮小)
	cacheOpts := cache.Options{}
	if watchNamespaces != "" {
		defaults := map[string]cache.Config{}
		for _, ns := range strings.Split(watchNamespaces, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				defaults[ns] = cache.Config{}
			}
		}
		cacheOpts.DefaultNamespaces = defaults
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsOpts,
		Cache:   cacheOpts,
		// SecretはキャッシュせずにAPI直読(cluster全Secretのinformerキャッシュ肥大を防ぐ)
		// 併せてSecretのwatch/Ownsを外しRBACのlist/watch権限も落とす(get中心へ縮小)
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			TLSOpts: []func(*tls.Config){disableHTTP2},
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cloudnative-misskey.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	digests := controller.NewDigestResolver()
	if err = (&controller.MisskeyReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorder("cloudnative-misskey"),
		DriftResyncInterval: driftResyncInterval,
		Digests:             digests,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Misskey")
		os.Exit(1)
	}
	if err = (&controller.MisskeyChannelReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Digests: digests,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MisskeyChannel")
		os.Exit(1)
	}

	// webhookはcert必須のためlocal実行等ではENABLE_WEBHOOKS=falseで無効化
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		imageAllow := splitCSV(allowedImageRegistries)
		issuerAllow := splitCSV(allowedClusterIssuers)
		if err := webhookv1beta1.SetupMisskeyWebhookWithManager(mgr, imageAllow, issuerAllow); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Misskey")
			os.Exit(1)
		}
		if err := webhookv1beta1.SetupMisskeyChannelWebhookWithManager(mgr, imageAllow); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "MisskeyChannel")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
