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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MisskeySpec defines the desired state of a Misskey instance.
// +kubebuilder:validation:XValidation:rule="has(self.image) != has(self.imageFrom)",message="exactly one of image or imageFrom must be set"
type MisskeySpec struct {
	// URL is the public-facing URL of the instance, e.g. https://misskey.example.com/.
	// Immutable after initialization (enforced by CEL, independent of the webhook).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="url is immutable"
	URL string `json:"url"`

	// Image is the Misskey server container image. The app and worker share it.
	// Exactly one of image or imageFrom must be set.
	// +optional
	Image string `json:"image,omitempty"`

	// ImageFrom resolves the image from a MisskeyChannel, enabling staged
	// fleet-wide rollouts. Exactly one of image or imageFrom must be set.
	// +optional
	ImageFrom *ImageFromSource `json:"imageFrom,omitempty"`

	// TrackImageDigest makes the operator resolve the image tag to its digest
	// against the registry and pin pods to image@digest, re-resolving
	// periodically. Content pushed under the same (mutable) tag — e.g. latest —
	// then rolls app/worker automatically. Uses imagePullSecrets for private
	// registries.
	// +optional
	TrackImageDigest bool `json:"trackImageDigest,omitempty"`

	// IDGenerationMethod is Misskey's note/user id format. Immutable after the
	// instance has been initialized; when migrating an existing database, set it
	// to the value the database was created with.
	// +kubebuilder:validation:Enum=aid;aidx;meid;ulid;objectid
	// +kubebuilder:default=aidx
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="idGenerationMethod is immutable"
	// +optional
	IDGenerationMethod string `json:"idGenerationMethod,omitempty"`

	// DeletionPolicy controls what happens to data-bearing resources (the CNPG
	// cluster, Redis/MeiliSearch StatefulSets and generated key Secrets) when this
	// Misskey is deleted. Retain (default) orphans them so the data survives;
	// recreating the Misskey with the same name re-adopts them. Delete
	// garbage-collects everything via owner references (destroys the database).
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// Suspend scales the app and worker Deployments to zero and pauses new
	// migration/objectStorage Jobs. The proxy (serving the maintenance page),
	// database, Redis and MeiliSearch keep running so data stays intact.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Tenant is the tenant identifier stamped as the cloudnative-misskey.dev/tenant
	// label on every managed resource and pod (incl. CNPG pods), for per-tenant log
	// and metric routing. Immutable; defaults to the namespace when omitted. Setting
	// it after creation (from unset) also relabels existing pods on the next
	// reconcile and shifts the tenant key, so fix it at creation time.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenant is immutable"
	// +optional
	Tenant string `json:"tenant,omitempty"`

	// ImagePullSecrets used to pull the Misskey image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Runtime adapts the operator to Misskey forks whose image contract (uid,
	// commands, paths, health endpoint) differs from upstream misskey/misskey.
	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// App configures the web/API server Deployment (runs with MK_ONLY_SERVER).
	// +optional
	App AppSpec `json:"app,omitempty"`

	// Worker configures the job queue Deployment (runs with MK_ONLY_QUEUE).
	// +optional
	Worker WorkerSpec `json:"worker,omitempty"`

	// Proxy configures the Caddy reverse proxy fronting the app.
	// +optional
	Proxy ProxySpec `json:"proxy,omitempty"`

	// Ingress configures the Ingress exposing the proxy (or the app when the
	// proxy is disabled).
	// +optional
	Ingress IngressSpec `json:"ingress,omitempty"`

	// Redis configures the Redis backend (managed StatefulSet or external).
	// +optional
	Redis RedisSpec `json:"redis,omitempty"`

	// Search configures the fulltext search provider. MeiliSearch by default.
	// +optional
	Search SearchSpec `json:"search,omitempty"`

	// Postgres configures the PostgreSQL backend (CNPG-managed or external).
	// Defaults to {} so the postgres key always exists and the recovery
	// immutability rule is always evaluated on updates.
	// +kubebuilder:default={}
	// +optional
	Postgres PostgresSpec `json:"postgres,omitempty"`

	// Migration configures the schema-migration Job behavior.
	// +optional
	Migration MigrationSpec `json:"migration,omitempty"`

	// SetupPassword configures the initial-setup admin password written to
	// default.yml as `setupPassword`. When the block is present with no
	// secretRef, the operator generates a random password into the Secret
	// "<name>-setup" (key SETUP_PASSWORD); retrieve it to complete first setup.
	// Omit the block to leave setupPassword out of default.yml entirely.
	// +optional
	SetupPassword *SetupPasswordSpec `json:"setupPassword,omitempty"`

	// ExtraConfig is raw YAML appended verbatim to the generated default.yml.
	// Use it for settings not modeled by this CRD.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	ExtraConfig string `json:"extraConfig,omitempty"`

	// ObjectStorage configures S3/R2-compatible media storage. Misskey stores this
	// in the DB meta table (not default.yml), so the operator writes it via a
	// one-shot Job after migration. Opt-in. Existing uploads are not migrated.
	// +optional
	ObjectStorage *ObjectStorageSpec `json:"objectStorage,omitempty"`

	// Network groups this instance's NetworkPolicy controls (ingress isolation and
	// opt-in egress isolation).
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Tenancy configures namespace-scoped isolation, assuming the namespace is
	// dedicated to this instance.
	// +optional
	Tenancy TenancySpec `json:"tenancy,omitempty"`

	// Monitoring generates Prometheus ServiceMonitor/PodMonitor for the instance's
	// managed backends (PostgreSQL/Redis/MeiliSearch).
	// +optional
	Monitoring MonitoringSpec `json:"monitoring,omitempty"`

	// Performance tunes the job-queue processor via default.yml. All fields are
	// optional; unset ones fall back to Misskey's own defaults (not written out).
	// +optional
	Performance PerformanceSpec `json:"performance,omitempty"`

	// OutboundProxy routes Misskey's outgoing HTTP(S)/SMTP through a forward proxy
	// (default.yml proxy/proxySmtp/proxyBypassHosts). Distinct from spec.proxy,
	// which is the inbound Caddy reverse proxy.
	// +optional
	OutboundProxy OutboundProxySpec `json:"outboundProxy,omitempty"`

	// Files tunes media/file handling written to default.yml (max upload size,
	// remote-file proxying, media proxy).
	// +optional
	Files FilesSpec `json:"files,omitempty"`
}

// PerformanceSpec tunes the job-queue processor. Each field maps to the matching
// default.yml key and is emitted only when set; omit to use Misskey's default.
type PerformanceSpec struct {
	// DeliverJobConcurrency caps concurrent activity deliveries per worker.
	// +kubebuilder:validation:Minimum=1
	// +optional
	DeliverJobConcurrency *int32 `json:"deliverJobConcurrency,omitempty"`

	// InboxJobConcurrency caps concurrent inbox (received activity) processing per worker.
	// +kubebuilder:validation:Minimum=1
	// +optional
	InboxJobConcurrency *int32 `json:"inboxJobConcurrency,omitempty"`

	// DeliverJobPerSec rate-limits enqueued deliver jobs per second.
	// +kubebuilder:validation:Minimum=1
	// +optional
	DeliverJobPerSec *int32 `json:"deliverJobPerSec,omitempty"`

	// InboxJobPerSec rate-limits enqueued inbox jobs per second.
	// +kubebuilder:validation:Minimum=1
	// +optional
	InboxJobPerSec *int32 `json:"inboxJobPerSec,omitempty"`

	// RelationshipJobPerSec rate-limits follow/unfollow relationship jobs per second.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RelationshipJobPerSec *int32 `json:"relationshipJobPerSec,omitempty"`

	// DeliverJobMaxAttempts is the retry count for a failed deliver job.
	// +kubebuilder:validation:Minimum=1
	// +optional
	DeliverJobMaxAttempts *int32 `json:"deliverJobMaxAttempts,omitempty"`

	// InboxJobMaxAttempts is the retry count for a failed inbox job.
	// +kubebuilder:validation:Minimum=1
	// +optional
	InboxJobMaxAttempts *int32 `json:"inboxJobMaxAttempts,omitempty"`
}

// OutboundProxySpec configures Misskey's forward proxy for outgoing traffic.
type OutboundProxySpec struct {
	// HTTP is the forward proxy URL for outgoing HTTP(S), e.g. http://proxy:3128
	// (default.yml proxy).
	// +optional
	HTTP string `json:"http,omitempty"`

	// SMTP is a separate forward proxy URL for outgoing SMTP (default.yml proxySmtp).
	// +optional
	SMTP string `json:"smtp,omitempty"`

	// BypassHosts are hosts reached directly, bypassing the proxy (default.yml
	// proxyBypassHosts), e.g. captcha or translation endpoints.
	// +optional
	BypassHosts []string `json:"bypassHosts,omitempty"`
}

// FilesSpec tunes media/file handling in default.yml.
type FilesSpec struct {
	// MaxFileSize is the maximum upload size in bytes (default.yml maxFileSize).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxFileSize *int64 `json:"maxFileSize,omitempty"`

	// MediaProxy is the URL of an external media proxy for remote files
	// (default.yml mediaProxy).
	// +optional
	MediaProxy string `json:"mediaProxy,omitempty"`

	// ProxyRemoteFiles proxies remote files through this server (default.yml
	// proxyRemoteFiles). Defaults to true when unset.
	// +optional
	ProxyRemoteFiles *bool `json:"proxyRemoteFiles,omitempty"`
}

// MonitoringSpec configures Prometheus scraping of the managed backends. Requires
// the Prometheus Operator CRDs (ServiceMonitor/PodMonitor). Opt-in.
type MonitoringSpec struct {
	// Enabled generates ServiceMonitor/PodMonitor and turns on each backend's
	// metrics endpoint (CNPG :9187, a redis_exporter sidecar, MeiliSearch metrics).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Labels are added to the generated ServiceMonitor/PodMonitor so your Prometheus
	// serviceMonitorSelector/podMonitorSelector matches them (e.g. release: kps).
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Interval is the scrape interval as a Prometheus duration, e.g. 30s, 1m.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Pattern=`^([0-9]+(ms|s|m|h|d|w|y))+$`
	// +optional
	Interval string `json:"interval,omitempty"`

	// RedisExporterImage is the redis_exporter sidecar image for standalone Redis.
	// Defaults on the operator side (not persisted into the CR), so operator
	// upgrades can evolve it fleet-wide.
	// +optional
	RedisExporterImage string `json:"redisExporterImage,omitempty"`

	// Rules generates a PrometheusRule with baseline alerts (proxy 5xx ratio,
	// backup staleness when postgres.backup is set). On by default while
	// monitoring is enabled.
	// +optional
	Rules *MonitoringRules `json:"rules,omitempty"`
}

// MonitoringRules configures the generated PrometheusRule.
type MonitoringRules struct {
	// Enabled toggles the PrometheusRule generation.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// BackupMaxAge is how old the latest base backup may become before the
	// MisskeyBackupStale alert fires.
	// +kubebuilder:default="48h"
	// +optional
	BackupMaxAge metav1.Duration `json:"backupMaxAge,omitempty"`
}

// MigrationSpec configures the schema-migration Job.
type MigrationSpec struct {
	// CreateIndexConcurrently sets MISSKEY_MIGRATION_CREATE_INDEX_CONCURRENTLY=1 on the
	// migration Job so index-creating migrations use CREATE INDEX CONCURRENTLY (which
	// does not block writes) instead of a locking CREATE INDEX, keeping deploys
	// non-blocking on large tables such as note. Default false (opt-in), matching
	// Misskey's upstream default of running all migrations in a single atomic
	// transaction. Tradeoffs when true: per-migration transactions (not one atomic
	// batch), slower index builds, and a failed concurrent build can leave an invalid
	// index needing manual cleanup.
	// +optional
	CreateIndexConcurrently *bool `json:"createIndexConcurrently,omitempty"`

	// PreBackup gates each new migration Job on an on-demand CNPG Backup of
	// the database, so a failed one-way migration can be rolled back by
	// restoring a new instance via postgres.recovery. Requires managed
	// PostgreSQL with postgres.backup configured; otherwise it is a no-op.
	// +optional
	PreBackup *bool `json:"preBackup,omitempty"`
}

// NetworkSpec groups the instance's NetworkPolicy controls: ingress isolation
// (Isolation) and opt-in egress isolation (EgressIsolation).
type NetworkSpec struct {
	// Isolation configures the ingress NetworkPolicy for this instance's backend
	// pods (app/worker/redis/meilisearch).
	// +optional
	Isolation NetworkIsolationSpec `json:"isolation,omitempty"`

	// EgressIsolation configures egress isolation (opt-in). See EgressIsolationSpec.
	// +optional
	EgressIsolation EgressIsolationSpec `json:"egressIsolation,omitempty"`
}

// NetworkIsolationSpec configures the per-instance ingress NetworkPolicy. It
// limits ingress to the backend pods (app/worker/redis/meilisearch) to
// intra-instance traffic; the public entry (proxy, or app when the proxy is
// disabled) stays reachable. PostgreSQL is intentionally excluded and left to
// CNPG, whose operator needs cross-namespace access to the instance manager.
// Only effective on CNIs that enforce NetworkPolicy.
type NetworkIsolationSpec struct {
	// Enabled creates the isolation NetworkPolicy. Default true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// AllowedNamespaces are namespace names (matched by the
	// kubernetes.io/metadata.name label) permitted to reach the isolated backend
	// pods in addition to intra-instance traffic, e.g. a monitoring namespace for
	// Prometheus scraping.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// EgressIsolationSpec configures per-instance egress NetworkPolicies (opt-in).
// app/worker must reach the whole Fediverse, so egress cannot be restricted by
// destination; the value here is blocking private ranges (RFC1918, link-local
// metadata) and the cluster-internal network, i.e. SSRF and lateral-movement
// hardening. app/worker keep public-internet egress; redis/meilisearch/proxy/
// maintenance get intra-instance + DNS only. PostgreSQL is excluded (delegated
// to CNPG). Only effective on CNIs that enforce egress policy.
type EgressIsolationSpec struct {
	// Enabled creates the egress NetworkPolicies. Default false (opt-in).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// DNSNamespace runs the cluster DNS (CoreDNS), allowed on :53. Defaults to
	// kube-system.
	// +kubebuilder:default="kube-system"
	// +optional
	DNSNamespace string `json:"dnsNamespace,omitempty"`

	// AllowedNamespaces are additional egress-permitted namespace names on any
	// port, e.g. an in-cluster SMTP relay or object store.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// TenancySpec configures namespace-scoped tenant isolation. ResourceQuota and
// LimitRange are namespace-wide, so they are only created when the namespace is
// declared dedicated to this instance.
// +kubebuilder:validation:XValidation:rule="!has(self.quota) || !self.quota.exists(k, k == 'cpu' || k == 'memory' || k.startsWith('requests.') || k.startsWith('limits.')) || (has(self.limitRange) && has(self.limitRange.default) && has(self.limitRange.defaultRequest))",message="tenancy.quota with compute resources requires tenancy.limitRange.default and defaultRequest, otherwise pods without explicit requests are rejected by the quota"
type TenancySpec struct {
	// Dedicated declares the namespace is dedicated to this instance, which is
	// required to manage the namespace-scoped ResourceQuota/LimitRange below.
	// +optional
	Dedicated bool `json:"dedicated,omitempty"`

	// Quota, when set with Dedicated, becomes the ResourceQuota spec.hard
	// (e.g. cpu, memory, requests.cpu, limits.memory, pods).
	// +optional
	Quota corev1.ResourceList `json:"quota,omitempty"`

	// LimitRange, when set with Dedicated, creates a Container-scoped LimitRange.
	// +optional
	LimitRange *LimitRangeSpec `json:"limitRange,omitempty"`
}

// LimitRangeSpec is the Container limits applied namespace-wide.
type LimitRangeSpec struct {
	// Default container limits.
	// +optional
	Default corev1.ResourceList `json:"default,omitempty"`
	// DefaultRequest container requests.
	// +optional
	DefaultRequest corev1.ResourceList `json:"defaultRequest,omitempty"`
	// Max container limits.
	// +optional
	Max corev1.ResourceList `json:"max,omitempty"`
}

// RuntimeSpec adapts the operator to Misskey forks whose image contract differs
// from the upstream misskey/misskey image. Every field defaults to upstream, so
// a structurally compatible fork usually needs only spec.image. The rendered
// config is always Misskey's default.yml format.
type RuntimeSpec struct {
	// RunAsUser overrides the container uid. Default 991 (upstream misskey image).
	// uid 0 conflicts with the enforced RunAsNonRoot and would wedge the pod, so it is rejected.
	// +kubebuilder:validation:Minimum=1
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// StartCommand overrides the app/worker container command.
	// Default ["pnpm","run","start"].
	// +optional
	StartCommand []string `json:"startCommand,omitempty"`

	// MigrateCommand overrides the migration Job command.
	// Default ["pnpm","run","migrate"].
	// +optional
	MigrateCommand []string `json:"migrateCommand,omitempty"`

	// HealthPath overrides the app HTTP health/probe path. Default /api/server-info.
	// +optional
	HealthPath string `json:"healthPath,omitempty"`

	// ConfigPath overrides where the rendered default.yml is mounted in the
	// container. Default /misskey/.config/default.yml.
	// +optional
	ConfigPath string `json:"configPath,omitempty"`

	// BuiltPath is the pre-built assets directory copied to a writable emptyDir so
	// the image can write compile-config output. Default /misskey/built. Set to an
	// empty string to disable the copy for forks that do not need it.
	// +optional
	BuiltPath *string `json:"builtPath,omitempty"`
}

// SetupPasswordSpec configures the Misskey initial-setup password.
type SetupPasswordSpec struct {
	// SecretRef references an existing secret key holding the setup password.
	// When nil, the operator generates one into the Secret "<name>-setup".
	// +optional
	SecretRef *corev1.SecretKeySelector `json:"secretRef,omitempty"`
}

// ComponentSpec is the shared Deployment shape of the app and worker.
type ComponentSpec struct {
	// Replicas is the desired number of pods.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources describes compute requests/limits for the container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector constrains scheduling to matching nodes.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow scheduling onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// AppSpec configures the web/API server Deployment.
type AppSpec struct {
	ComponentSpec `json:",inline"`

	// Autoscaling replaces the static Replicas with a HorizontalPodAutoscaler
	// (CPU/memory, needs metrics-server) or, when rps is set, a KEDA
	// ScaledObject on the proxy's request rate.
	// +optional
	Autoscaling *AppAutoscalingSpec `json:"autoscaling,omitempty"`
}

// WorkerSpec configures the job queue Deployment.
type WorkerSpec struct {
	ComponentSpec `json:",inline"`

	// Autoscaling replaces the static Replicas with a HorizontalPodAutoscaler
	// (CPU/memory, needs metrics-server) or, when queues is set, a KEDA
	// ScaledObject on BullMQ queue depth.
	// +optional
	Autoscaling *WorkerAutoscalingSpec `json:"autoscaling,omitempty"`
}

// AutoscalingSpec is the replica range and resource metrics shared by the app
// and worker autoscaling blocks. Presence of a component's autoscaling block
// enables autoscaling; omit it for static replicas.
type AutoscalingSpec struct {
	// MinReplicas is the lower bound. Default 1; use >=2 so the PodDisruptionBudget
	// (maxUnavailable=1) still allows node drains.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// TargetCPUUtilizationPercentage adds a CPU utilization metric. Unset omits it.
	// +optional
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// TargetMemoryUtilizationPercentage adds a memory utilization metric. Unset omits it.
	// +optional
	TargetMemoryUtilizationPercentage *int32 `json:"targetMemoryUtilizationPercentage,omitempty"`
}

// AppAutoscalingSpec autoscales the app Deployment. When RPS is unset a native
// HPA is created (CPU/memory); when RPS is set a KEDA ScaledObject scales on
// the proxy's request rate.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type AppAutoscalingSpec struct {
	AutoscalingSpec `json:",inline"`

	// RPS, when set, switches the mechanism to a KEDA ScaledObject that scales
	// on the proxy's request rate via a prometheus trigger. Requires KEDA and
	// monitoring.enabled (or an equivalent scrape) so the Caddy metrics are
	// collected.
	// +optional
	RPS *RPSTrigger `json:"rps,omitempty"`
}

// WorkerAutoscalingSpec autoscales the worker Deployment. When Queues is empty
// a native HPA is created (CPU/memory); when Queues is set a KEDA ScaledObject
// scales on BullMQ wait-list depth.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type WorkerAutoscalingSpec struct {
	AutoscalingSpec `json:",inline"`

	// Queues, when set, switches the mechanism to a KEDA ScaledObject that scales on
	// BullMQ queue wait-list depth. Requires KEDA in the cluster. Typically deliver
	// and inbox.
	// +optional
	Queues []QueueScaleTrigger `json:"queues,omitempty"`
}

// RPSTrigger scales on the proxy's request rate from Prometheus.
type RPSTrigger struct {
	// ServerAddress is the Prometheus endpoint KEDA queries, e.g.
	// http://prometheus.monitoring:9090.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	ServerAddress string `json:"serverAddress"`

	// TargetRPS is the request rate each replica should absorb.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	TargetRPS int32 `json:"targetRPS"`

	// Query overrides the default PromQL (this instance's total proxy RPS).
	// +optional
	Query string `json:"query,omitempty"`
}

// QueueScaleTrigger scales on the wait-list depth of one BullMQ queue.
type QueueScaleTrigger struct {
	// Name is the BullMQ queue name, e.g. deliver, inbox.
	Name string `json:"name"`

	// ListLength is the target average wait-list length per replica.
	// +kubebuilder:validation:Minimum=1
	ListLength int32 `json:"listLength"`

	// ListName overrides the exact Redis key watched. Defaults to the computed
	// BullMQ wait-list key for Name.
	// +optional
	ListName string `json:"listName,omitempty"`
}

// ProxySpec configures the Caddy reverse proxy and maintenance fallback.
type ProxySpec struct {
	// Enabled toggles the Caddy proxy. When false the Ingress targets the app directly.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Replicas of the proxy Deployment.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image for the Caddy proxy container. Defaults to caddy:2 on the operator
	// side (not persisted into the CR), so operator upgrades can evolve it
	// fleet-wide.
	// +optional
	Image string `json:"image,omitempty"`

	// Maintenance configures the fallback page served when the app is down.
	// +optional
	Maintenance MaintenanceSpec `json:"maintenance,omitempty"`

	// ClientIPHeader, when set, overrides X-Real-IP and X-Forwarded-For from the
	// named header (e.g. CF-Connecting-IP behind Cloudflare). When empty, the
	// upstream's X-Forwarded-For is preserved, trusting private-range upstreams
	// via Caddy trusted_proxies. Restricted to a bare HTTP header name so it cannot
	// inject arbitrary Caddyfile directives.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+$`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ClientIPHeader string `json:"clientIPHeader,omitempty"`

	// Resources for the proxy container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MaintenanceSpec configures the maintenance fallback served on backend errors.
type MaintenanceSpec struct {
	// Enabled toggles the maintenance fallback page served by the proxy itself.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// HTML is the page body served during maintenance. A default is used when empty.
	// +kubebuilder:validation:MaxLength=262144
	// +optional
	HTML string `json:"html,omitempty"`

	// StatusCode is the HTTP status returned for the maintenance page. Default 200
	// so the page renders as a normal 2xx response. Set to 503 for the standard
	// "temporarily unavailable" semantics. Note that /api/* is always excluded
	// from the maintenance page and returns the real backend status so external
	// health checks are not masked.
	// +kubebuilder:default=200
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	// +optional
	StatusCode *int32 `json:"statusCode,omitempty"`

	// ReloadSeconds sets the auto-reload interval of the built-in maintenance
	// page, in seconds. 0 disables auto-reload. Ignored when a custom html is set.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReloadSeconds *int32 `json:"reloadSeconds,omitempty"`
}

// IngressSpec configures the Ingress exposing the instance.
type IngressSpec struct {
	// Enabled toggles Ingress creation.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// ClassName sets ingressClassName, e.g. nginx or traefik.
	// +kubebuilder:default="nginx"
	// +optional
	ClassName string `json:"className,omitempty"`

	// Host overrides the ingress host. Defaults to the host part of spec.url.
	// Constrained to a DNS-1123 subdomain to prevent host takeover via malformed values.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Host string `json:"host,omitempty"`

	// Annotations added to the Ingress object. Operator-managed annotations
	// (cert-manager, proxy-body-size) always win, and known privilege-escalating
	// keys (nginx *-snippet, auth-url, ...) are dropped by the controller.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 4096)",message="ingress annotation values must be at most 4096 characters"
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// TLSSecretName enables a TLS block referencing the named secret. Optional.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`

	// IssuerRef points at a cert-manager Issuer/ClusterIssuer. The operator
	// stamps the cert-manager annotation and a TLS block (secret "<name>-tls"
	// unless tlsSecretName overrides it) so the certificate is provisioned
	// automatically. Requires cert-manager in the cluster.
	// +optional
	IssuerRef *IngressIssuerRef `json:"issuerRef,omitempty"`
}

// IngressIssuerRef references a cert-manager issuer.
type IngressIssuerRef struct {
	// Name of the issuer.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Kind of the issuer.
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default=ClusterIssuer
	// +optional
	Kind string `json:"kind,omitempty"`
}

// RedisSpec configures the Redis backend.
// +kubebuilder:validation:XValidation:rule="!(has(self.external) && has(self.ha))",message="HA requires managed Redis; remove redis.external"
type RedisSpec struct {
	// External points Misskey at an existing Redis. When set, no Redis is managed.
	// +optional
	External *ExternalRedis `json:"external,omitempty"`

	// Image for the managed standalone Redis StatefulSet (HA off). Redis 8 is
	// AGPLv3-licensed, matching this project.
	// +kubebuilder:default="redis:8-alpine"
	// +optional
	Image string `json:"image,omitempty"`

	// MaxMemory passes --maxmemory to redis-server, e.g. 400mb, 1gb, or bytes.
	// +kubebuilder:default="400mb"
	// +kubebuilder:validation:Pattern=`^[0-9]+([kKmMgG][bB]?|[bB])?$`
	// +optional
	MaxMemory string `json:"maxMemory,omitempty"`

	// MaxMemoryPolicy sets --maxmemory-policy. Default noeviction, because this
	// Redis also backs the job queue (BullMQ) and an eviction policy such as
	// allkeys-lru would silently drop queued/delayed jobs under memory pressure.
	// +kubebuilder:validation:Enum=noeviction;allkeys-lru;allkeys-lfu;allkeys-random;volatile-lru;volatile-lfu;volatile-random;volatile-ttl
	// +kubebuilder:default=noeviction
	// +optional
	MaxMemoryPolicy string `json:"maxMemoryPolicy,omitempty"`

	// AppendOnly enables Redis AOF (--appendonly yes) for job-queue durability
	// across restarts. Default true; set false for pure-cache use.
	// +kubebuilder:default=true
	// +optional
	AppendOnly *bool `json:"appendOnly,omitempty"`

	// Storage size of the managed Redis PVC.
	// +kubebuilder:default="2Gi"
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// StorageClassName for the managed Redis PVC.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Resources for the managed Redis container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// HA enables operator-managed Sentinel HA (CloudNativePG-style, via the
	// OT-CONTAINER-KIT redis-operator) for managed Redis instances instead of a
	// single-pod StatefulSet. Requires the redis-operator installed in the cluster.
	// +optional
	HA *RedisHA `json:"ha,omitempty"`

	// Roles configures per-role Redis separation. Each role (jobQueue, pubsub,
	// timelines, reactions) can point at its own managed instance or an external
	// Redis; an unset role falls back to the shared redis (Misskey omits the
	// redisForXxx block, which makes Misskey reuse the main redis).
	// +optional
	Roles *RedisRoles `json:"roles,omitempty"`
}

// RedisHA configures operator-managed Sentinel high availability. Presence of
// the block enables HA; omit it for a single-pod Redis.
type RedisHA struct {
	// Replicas is the RedisReplication cluster size: total redis nodes
	// (1 primary + N-1 replicas). 3 gives one primary and two replicas.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Sentinels is the RedisSentinel cluster size. Use an odd number for quorum.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	Sentinels int32 `json:"sentinels,omitempty"`

	// Image is the redis image driven by the operator's RedisReplication.
	// +kubebuilder:default="quay.io/opstree/redis:v8.2.5"
	// +optional
	Image string `json:"image,omitempty"`

	// SentinelImage is the redis-sentinel image for the operator's RedisSentinel.
	// +kubebuilder:default="quay.io/opstree/redis-sentinel:v8.2.5"
	// +optional
	SentinelImage string `json:"sentinelImage,omitempty"`
}

// RedisRoles configures per-role Redis separation.
type RedisRoles struct {
	// +optional
	JobQueue *RedisRole `json:"jobQueue,omitempty"`
	// +optional
	Pubsub *RedisRole `json:"pubsub,omitempty"`
	// +optional
	Timelines *RedisRole `json:"timelines,omitempty"`
	// +optional
	Reactions *RedisRole `json:"reactions,omitempty"`
}

// RedisRole separates one Misskey Redis role. Presence separates the role;
// External points it at an existing Redis, otherwise a dedicated instance is
// provisioned with the managed-override fields.
// +kubebuilder:validation:XValidation:rule="!has(self.external) || !(has(self.ha) || has(self.maxMemory) || has(self.maxMemoryPolicy) || has(self.storage) || has(self.storageClassName) || has(self.resources))",message="external role cannot also set HA or managed overrides (maxMemory/storage/etc.)"
type RedisRole struct {
	// External points this role at an existing Redis. Mutually exclusive with the
	// managed override fields.
	// +optional
	External *ExternalRedis `json:"external,omitempty"`

	// MaxMemory override for this role's managed instance, e.g. 400mb, 1gb.
	// +kubebuilder:validation:Pattern=`^[0-9]+([kKmMgG][bB]?|[bB])?$`
	// +optional
	MaxMemory string `json:"maxMemory,omitempty"`

	// MaxMemoryPolicy override for this role's managed instance.
	// +kubebuilder:validation:Enum=noeviction;allkeys-lru;allkeys-lfu;allkeys-random;volatile-lru;volatile-lfu;volatile-random;volatile-ttl
	// +optional
	MaxMemoryPolicy string `json:"maxMemoryPolicy,omitempty"`

	// Storage override for this role's managed PVC.
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// StorageClassName override for this role's managed PVC.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Resources override for this role's managed container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// HA turns this role's managed instance into a Sentinel HA setup. Presence
	// enables HA for this role independently; omit for a single-pod instance.
	// (spec.redis.ha applies to the shared default redis, not to roles.)
	// +optional
	HA *RedisHA `json:"ha,omitempty"`
}

// ExternalRedis references a Redis running outside the operator's control.
// +kubebuilder:validation:XValidation:rule="!has(self.sentinels) || size(self.sentinels) == 0 || has(self.masterName)",message="masterName is required when sentinels is set"
type ExternalRedis struct {
	// Host of the external Redis. With Sentinels set this is ignored by the
	// client but still required by Misskey's config schema.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port of the external Redis.
	// +kubebuilder:default=6379
	// +optional
	Port int32 `json:"port,omitempty"`

	// DB is the Redis logical database index. Default 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DB int32 `json:"db,omitempty"`

	// PasswordSecret optionally references the Redis password.
	// +optional
	PasswordSecret *corev1.SecretKeySelector `json:"passwordSecret,omitempty"`

	// Sentinels, when set, connects via Redis Sentinel (ioredis passthrough)
	// instead of host/port.
	// +optional
	Sentinels []RedisHostPort `json:"sentinels,omitempty"`

	// MasterName is the sentinel master group name (required with Sentinels).
	// +optional
	MasterName string `json:"masterName,omitempty"`

	// TLS enables TLS for connections to this external Redis. Also propagated to
	// the KEDA autoscaling trigger so it does not connect in plaintext.
	// +optional
	TLS *bool `json:"tls,omitempty"`
}

// RedisHostPort is a sentinel endpoint.
type RedisHostPort struct {
	// +kubebuilder:validation:Required
	Host string `json:"host"`
	// +kubebuilder:default=26379
	// +optional
	Port int32 `json:"port,omitempty"`
}

// SearchProvider selects the Misskey fulltext search backend.
// +kubebuilder:validation:Enum=meilisearch;sqlLike;sqlPgroonga
type SearchProvider string

const (
	// SearchMeilisearch uses MeiliSearch (the default).
	SearchMeilisearch SearchProvider = "meilisearch"
	// SearchSQLLike uses PostgreSQL LIKE (no extension required).
	SearchSQLLike SearchProvider = "sqlLike"
	// SearchSQLPgroonga uses the PGroonga PostgreSQL extension.
	SearchSQLPgroonga SearchProvider = "sqlPgroonga"
)

// SearchSpec configures fulltext search.
type SearchSpec struct {
	// Provider selects the fulltext search backend.
	// +kubebuilder:default=meilisearch
	// +optional
	Provider SearchProvider `json:"provider,omitempty"`

	// Meilisearch configures the MeiliSearch backend (provider=meilisearch).
	// +optional
	Meilisearch MeilisearchSpec `json:"meilisearch,omitempty"`
}

// MeilisearchSpec configures the MeiliSearch backend.
type MeilisearchSpec struct {
	// External points Misskey at an existing MeiliSearch. When set, none is managed.
	// +optional
	External *ExternalMeilisearch `json:"external,omitempty"`

	// Image for the managed MeiliSearch StatefulSet.
	// +kubebuilder:default="getmeili/meilisearch:v1"
	// +optional
	Image string `json:"image,omitempty"`

	// Storage size of the managed MeiliSearch PVC.
	// +kubebuilder:default="10Gi"
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// StorageClassName for the managed MeiliSearch PVC.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Index name used by Misskey. Defaults to the sanitized host of spec.url.
	// +optional
	Index string `json:"index,omitempty"`

	// Scope selects which notes are indexed: local (this server) or global.
	// +kubebuilder:validation:Enum=local;global
	// +kubebuilder:default=local
	// +optional
	Scope string `json:"scope,omitempty"`

	// MasterKeySecret references an existing master key. When empty the operator
	// generates one and stores it in a Secret named <misskey>-meilisearch.
	// +optional
	MasterKeySecret *corev1.SecretKeySelector `json:"masterKeySecret,omitempty"`

	// Resources for the managed MeiliSearch container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExternalMeilisearch references a MeiliSearch running outside the operator.
type ExternalMeilisearch struct {
	// Host of the external MeiliSearch.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port of the external MeiliSearch.
	// +kubebuilder:default=7700
	// +optional
	Port int32 `json:"port,omitempty"`

	// SSL toggles https when talking to the external MeiliSearch.
	// +optional
	SSL bool `json:"ssl,omitempty"`

	// APIKeySecret references the API key for the external MeiliSearch.
	// +kubebuilder:validation:Required
	APIKeySecret corev1.SecretKeySelector `json:"apiKeySecret"`
}

// PostgresSpec configures the PostgreSQL backend.
// +kubebuilder:validation:XValidation:rule="!(has(self.external) && has(self.pooler))",message="pooler requires managed PostgreSQL; remove postgres.external"
// +kubebuilder:validation:XValidation:rule="!(has(self.external) && has(self.backup))",message="backup requires managed PostgreSQL; remove postgres.external"
// +kubebuilder:validation:XValidation:rule="!(has(self.external) && has(self.recovery))",message="recovery requires managed PostgreSQL; remove postgres.external"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.recovery) == has(self.recovery) && (!has(self.recovery) || self.recovery == oldSelf.recovery)",message="postgres.recovery is immutable: it declares the instance's origin at creation and cannot be added, changed or removed afterwards"
// +kubebuilder:validation:XValidation:rule="!(has(self.external) && has(self.__import__))",message="import requires managed PostgreSQL; remove postgres.external"
// +kubebuilder:validation:XValidation:rule="!(has(self.recovery) && has(self.__import__))",message="import and recovery are mutually exclusive bootstrap sources"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.__import__) == has(self.__import__) && (!has(self.__import__) || self.__import__ == oldSelf.__import__)",message="postgres.import is immutable: it declares the instance's origin at creation and cannot be added, changed or removed afterwards"
// +kubebuilder:validation:XValidation:rule="!(has(self.backup) && has(self.recovery)) || self.backup.destinationPath != self.recovery.source.destinationPath || (has(self.backup.serverName) && self.backup.serverName != self.recovery.source.serverName)",message="backup would overwrite the recovery source WAL archive; set postgres.backup.serverName different from recovery.source.serverName when sharing destinationPath"
type PostgresSpec struct {
	// External points Misskey at an existing PostgreSQL. When set, CNPG is not used.
	// +optional
	External *ExternalPostgres `json:"external,omitempty"`

	// Instances is the CNPG cluster size (1 = single, >=2 = HA with replicas).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// Image is the CNPG PostgreSQL image (CNPG imageName). The default is
	// persisted at creation so the PostgreSQL major version stays pinned for
	// the cluster's lifetime regardless of operator upgrades.
	// +kubebuilder:default="ghcr.io/cloudnative-pg/postgresql:17"
	// +optional
	Image string `json:"image,omitempty"`

	// Database is the DB name created by CNPG initdb.
	// +kubebuilder:default="misskey"
	// +optional
	Database string `json:"database,omitempty"`

	// Owner is the DB owner role created by CNPG initdb.
	// +kubebuilder:default="misskey"
	// +optional
	Owner string `json:"owner,omitempty"`

	// Storage size of each CNPG instance PVC.
	// +kubebuilder:default="20Gi"
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// StorageClassName for the CNPG PVCs.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Parameters merged into postgresql.parameters of the CNPG cluster.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// Resources for each CNPG instance.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Backup enables barmanObjectStore backups on the CNPG cluster.
	// +optional
	Backup *PostgresBackup `json:"backup,omitempty"`

	// Recovery bootstraps the CNPG cluster from an existing barmanObjectStore
	// backup instead of initdb (disaster recovery / instance migration). Only
	// honored at CR creation since CNPG evaluates bootstrap once; immutable and
	// inert afterwards — leave it in the manifest.
	// +optional
	Recovery *PostgresRecovery `json:"recovery,omitempty"`

	// Import bootstraps the CNPG cluster by logically importing a running
	// external PostgreSQL via pg_dump/pg_restore (CNPG initdb.import, for
	// instance migration without an object store). Only honored at CR
	// creation; immutable and inert afterwards — leave it in the manifest.
	// +optional
	Import *PostgresImport `json:"import,omitempty"`

	// ReadOffload wires Misskey dbReplications onto CNPG standby replicas so reads
	// are load-balanced off the primary. Defaults on when instances >= 2; set false
	// to opt out.
	// +optional
	ReadOffload *bool `json:"readOffload,omitempty"`

	// Pooler provisions CNPG PgBouncer connection poolers in front of the cluster,
	// multiplexing app/worker connections so max_connections is not exhausted as
	// pods scale out.
	// +optional
	Pooler *PostgresPooler `json:"pooler,omitempty"`
}

// PostgresPooler configures CNPG PgBouncer poolers. Presence enables them; the
// operator creates a read-write pooler and, when read offload is active, a
// read-only pooler.
type PostgresPooler struct {
	// Instances is the PgBouncer pod count per pooler.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// PoolMode is the PgBouncer pool mode: transaction, session or statement.
	// +kubebuilder:validation:Enum=transaction;session;statement
	// +kubebuilder:default=transaction
	// +optional
	PoolMode string `json:"poolMode,omitempty"`

	// Parameters merged into pgbouncer.parameters, e.g. max_client_conn,
	// default_pool_size.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// Resources for each PgBouncer pod.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExternalPostgres references a PostgreSQL running outside the operator.
type ExternalPostgres struct {
	// Host of the external PostgreSQL.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port of the external PostgreSQL.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database name.
	// +kubebuilder:validation:Required
	Database string `json:"database"`

	// User name.
	// +kubebuilder:validation:Required
	User string `json:"user"`

	// PasswordSecret references the DB password.
	// +kubebuilder:validation:Required
	PasswordSecret corev1.SecretKeySelector `json:"passwordSecret"`
}

// PostgresBackup configures CNPG barmanObjectStore backups.
type PostgresBackup struct {
	// DestinationPath is the object store path, e.g. s3://bucket/path.
	// +kubebuilder:validation:Required
	DestinationPath string `json:"destinationPath"`

	// EndpointURL of the S3-compatible object store.
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`

	// S3Credentials references the access/secret keys.
	// +optional
	S3Credentials *S3Credentials `json:"s3Credentials,omitempty"`

	// RetentionPolicy for backups, e.g. "30d".
	// +kubebuilder:default="30d"
	// +optional
	RetentionPolicy string `json:"retentionPolicy,omitempty"`

	// Schedule is a 6-field cron (with seconds) for a CNPG ScheduledBackup, e.g.
	// "0 0 3 * * *". Empty disables it.
	// +kubebuilder:validation:Pattern=`^(\S+\s+){5}\S+$`
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// ServerName overrides the barman folder name under destinationPath
	// (defaults to the cluster name "<name>-db"). Required to differ from
	// recovery.source.serverName when sharing its destinationPath, so the new
	// cluster does not overwrite the origin WAL archive.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// Verify schedules periodic restore tests: a throwaway CNPG cluster is
	// bootstrapped from the latest backup, checked for readiness and deleted.
	// The outcome lands in status.backupVerification.
	// +optional
	Verify *BackupVerify `json:"verify,omitempty"`
}

// BackupVerify configures periodic restore tests of the backups.
type BackupVerify struct {
	// Interval between restore tests.
	// +kubebuilder:default="168h"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// Timeout for a single restore test before it is recorded as failed.
	// +kubebuilder:default="30m"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// PostgresRecovery bootstraps the managed CNPG cluster from an existing
// barmanObjectStore backup instead of initdb.
type PostgresRecovery struct {
	// Source is the barmanObjectStore location of the origin cluster's backup.
	// +kubebuilder:validation:Required
	Source RecoverySource `json:"source"`

	// TargetTime is an optional RFC3339 timestamp for point-in-time recovery.
	// Omit to recover to the latest available WAL. CNPG picks the closest
	// backup completed before the target.
	// +kubebuilder:validation:Format=date-time
	// +optional
	TargetTime string `json:"targetTime,omitempty"`
}

// RecoverySource locates the origin cluster's backup in an object store.
type RecoverySource struct {
	// DestinationPath is the object store path of the backups, e.g. s3://bucket/path.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	DestinationPath string `json:"destinationPath"`

	// EndpointURL of the S3-compatible object store.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`

	// ServerName is the origin cluster's folder name under destinationPath,
	// usually "<old CR name>-db".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ServerName string `json:"serverName"`

	// S3Credentials references the access/secret keys.
	// +optional
	S3Credentials *S3Credentials `json:"s3Credentials,omitempty"`
}

// PostgresImport bootstraps the managed CNPG cluster by logically importing an
// existing PostgreSQL database.
type PostgresImport struct {
	// Source is the running PostgreSQL to import from. It must stay reachable
	// until the import completes.
	// +kubebuilder:validation:Required
	Source ImportSource `json:"source"`
}

// ImportSource references a running external PostgreSQL to import from.
type ImportSource struct {
	// Host of the source PostgreSQL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Host string `json:"host"`

	// Port of the source PostgreSQL.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database name to import.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Database string `json:"database"`

	// User name on the source.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	User string `json:"user"`

	// PasswordSecret references the source user's password.
	// +kubebuilder:validation:Required
	PasswordSecret corev1.SecretKeySelector `json:"passwordSecret"`

	// SSLMode for the connection to the source.
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default=prefer
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
}

// S3Credentials references S3-compatible credentials stored in secrets.
type S3Credentials struct {
	// AccessKeyID references the access key id.
	AccessKeyID corev1.SecretKeySelector `json:"accessKeyId"`
	// SecretAccessKey references the secret access key.
	SecretAccessKey corev1.SecretKeySelector `json:"secretAccessKey"`
}

// ObjectStorageSpec configures S3/R2-compatible media storage. The operator
// writes these into Misskey's meta table via a one-shot Job (autoConfigure),
// since Misskey does not read object storage settings from default.yml. Generic
// S3; for Cloudflare R2 see the README example (endpoint without scheme,
// region auto, baseUrl a public domain, setPublicRead false).
// +kubebuilder:validation:XValidation:rule="!has(self.extraColumns) || self.extraColumns.all(k, k.matches('^[A-Za-z_][A-Za-z0-9_]*$'))",message="extraColumns keys must be valid SQL identifiers"
type ObjectStorageSpec struct {
	// Bucket is the S3 bucket name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Endpoint is the S3 API endpoint host without a scheme, e.g.
	// s3.example.com or <accountid>.r2.cloudflarestorage.com. The scheme is
	// derived from useSSL.
	// +kubebuilder:validation:Pattern=`^[^/]*$`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Region is the S3 region. Defaults to us-east-1 for stores with no region
	// concept (e.g. MinIO); Cloudflare R2 uses "auto".
	// +kubebuilder:default="us-east-1"
	// +optional
	Region string `json:"region,omitempty"`

	// Prefix is an optional key prefix for stored objects.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// BaseURL is the public base URL used to build file URLs, e.g. a bucket
	// public domain. Required for R2 (its S3 API endpoint is not public).
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +optional
	BaseURL string `json:"baseUrl,omitempty"`

	// Port overrides the endpoint port. Omit for the scheme default.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// UseSSL talks to the endpoint over https. Default true.
	// +kubebuilder:default=true
	// +optional
	UseSSL *bool `json:"useSSL,omitempty"`

	// UseProxy routes S3 traffic through Misskey's configured HTTP proxy. Default true.
	// +kubebuilder:default=true
	// +optional
	UseProxy *bool `json:"useProxy,omitempty"`

	// SetPublicRead sets a public-read ACL on upload. Default false. Must stay
	// false for Cloudflare R2, which does not support object ACLs.
	// +kubebuilder:default=false
	// +optional
	SetPublicRead *bool `json:"setPublicRead,omitempty"`

	// S3ForcePathStyle uses path-style requests against the endpoint. Default true.
	// +kubebuilder:default=true
	// +optional
	S3ForcePathStyle *bool `json:"s3ForcePathStyle,omitempty"`

	// Credentials references the S3 access key id and secret access key.
	// +kubebuilder:validation:Required
	Credentials S3Credentials `json:"credentials"`

	// AutoConfigure lets the operator write the settings into the meta table via
	// a Job. Default true. Set false to declare the settings without the operator
	// touching the database (apply them yourself).
	// +kubebuilder:default=true
	// +optional
	AutoConfigure *bool `json:"autoConfigure,omitempty"`

	// ColumnNames overrides the meta table column names, for Misskey forks or
	// older versions whose columns differ from upstream.
	// +optional
	ColumnNames *ObjectStorageColumns `json:"columnNames,omitempty"`

	// ExtraColumns writes additional meta columns (fork-specific) as plaintext
	// values keyed by column name. Secrets belong in credentials, not here.
	// +optional
	ExtraColumns map[string]string `json:"extraColumns,omitempty"`

	// Image is the psql-capable image for the meta-write Job. Defaults on the
	// operator side to the CNPG PostgreSQL image, which ships psql 16+
	// (required for \getenv).
	// +optional
	Image string `json:"image,omitempty"`
}

// ObjectStorageColumns overrides the meta table column identifiers. Each field
// defaults to the upstream Misskey column name; set one only when a fork or an
// older version renamed it. Values are SQL identifiers.
type ObjectStorageColumns struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	UseObjectStorage string `json:"useObjectStorage,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	BaseURL string `json:"baseUrl,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Bucket string `json:"bucket,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Region string `json:"region,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Port string `json:"port,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	AccessKey string `json:"accessKey,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SecretKey string `json:"secretKey,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	UseSSL string `json:"useSSL,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	UseProxy string `json:"useProxy,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SetPublicRead string `json:"setPublicRead,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	S3ForcePathStyle string `json:"s3ForcePathStyle,omitempty"`
}

// MisskeyStatus defines the observed state of a Misskey instance.
type MisskeyStatus struct {
	// Conditions represent the latest available observations of the instance state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase is a coarse state summary: Progressing (subsystems not all ready),
	// Running (all ready), or Error (reconcile failed).
	// +kubebuilder:validation:Enum=Progressing;Running;Error;Suspended
	// +optional
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DatabaseHost is the resolved PostgreSQL host apps connect to: the PgBouncer
	// pooler when enabled, the CNPG read-write service, or the external host.
	// +optional
	DatabaseHost string `json:"databaseHost,omitempty"`

	// RedisHost is the resolved Redis host (the operator's Sentinel-managed service
	// in HA mode).
	// +optional
	RedisHost string `json:"redisHost,omitempty"`

	// SearchIndex is the resolved MeiliSearch index name. Empty for non-meilisearch
	// providers.
	// +optional
	SearchIndex string `json:"searchIndex,omitempty"`

	// BackupVerification is the outcome of the last scheduled restore test
	// (postgres.backup.verify).
	// +optional
	BackupVerification *BackupVerificationStatus `json:"backupVerification,omitempty"`

	// Backup mirrors the CNPG backup status of the managed database.
	// +optional
	Backup *BackupStatus `json:"backup,omitempty"`

	// Image is the effective image the instance runs: spec.image, or the
	// channel-resolved image when spec.imageFrom is set.
	// +optional
	Image string `json:"image,omitempty"`
}

// ImageFromSource resolves the instance image from a fleet-level source.
type ImageFromSource struct {
	// Channel is the name of the cluster-scoped MisskeyChannel to follow.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Channel string `json:"channel"`
}

// BackupStatus mirrors CNPG's backup-related cluster status.
type BackupStatus struct {
	// LastSuccessfulBackup is when the latest base backup completed.
	// +optional
	LastSuccessfulBackup metav1.Time `json:"lastSuccessfulBackup,omitempty"`

	// FirstRecoverabilityPoint is the earliest available PITR target.
	// +optional
	FirstRecoverabilityPoint metav1.Time `json:"firstRecoverabilityPoint,omitempty"`
}

// BackupVerificationStatus records the last restore test of the backups.
type BackupVerificationStatus struct {
	// LastVerifiedTime is when the last restore test finished.
	// +optional
	LastVerifiedTime metav1.Time `json:"lastVerifiedTime,omitempty"`

	// Result of the last restore test.
	// +kubebuilder:validation:Enum=Succeeded;Failed
	// +optional
	Result string `json:"result,omitempty"`

	// Message carries failure details.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mk
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Search",type=string,JSONPath=`.spec.search.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.databaseHost`,priority=1
// +kubebuilder:printcolumn:name="LastBackup",type=date,JSONPath=`.status.backup.lastSuccessfulBackup`,priority=1
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`,priority=1
// +kubebuilder:printcolumn:name="Index",type=string,JSONPath=`.status.searchIndex`,priority=1

// Misskey is the Schema for the misskeys API.
type Misskey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MisskeySpec   `json:"spec,omitempty"`
	Status MisskeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MisskeyList contains a list of Misskey.
type MisskeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Misskey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Misskey{}, &MisskeyList{})
}
