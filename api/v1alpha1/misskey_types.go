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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MisskeySpec defines the desired state of a Misskey instance.
type MisskeySpec struct {
	// URL is the public-facing URL of the instance, e.g. https://misskey.example.com/.
	// Immutable after initialization (enforced by CEL, independent of the webhook).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="url is immutable"
	URL string `json:"url"`

	// Image is the Misskey server container image. The app and worker share it.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

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
	// Misskey is deleted. Delete (default) garbage-collects everything via owner
	// references. Retain orphans them so the data survives; recreating the Misskey
	// with the same name re-adopts them.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

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
	App ComponentSpec `json:"app,omitempty"`

	// Worker configures the job queue Deployment (runs with MK_ONLY_QUEUE).
	// +optional
	Worker ComponentSpec `json:"worker,omitempty"`

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
	// +optional
	ExtraConfig string `json:"extraConfig,omitempty"`

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
	// +kubebuilder:default="oliver006/redis_exporter:v1.62.0-alpine"
	// +optional
	RedisExporterImage string `json:"redisExporterImage,omitempty"`
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

// ComponentSpec is the shared shape of the app and worker Deployments.
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

	// Autoscaling replaces the static Replicas with a HorizontalPodAutoscaler
	// (CPU/memory) or, when queues are set, a KEDA ScaledObject (BullMQ queue
	// depth). HPA needs metrics-server; queue scaling needs KEDA installed.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

// AutoscalingSpec configures autoscaling for an app/worker Deployment. Presence
// of the block enables autoscaling; omit it for static replicas. When Queues is
// empty a native HPA is created (CPU/memory); when Queues is set a KEDA
// ScaledObject is created that scales on BullMQ wait-list depth.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
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

	// Queues, when set, switches the mechanism to a KEDA ScaledObject that scales on
	// BullMQ queue wait-list depth. Requires KEDA in the cluster. Meaningful for the
	// worker; typically deliver and inbox.
	// +optional
	Queues []QueueScaleTrigger `json:"queues,omitempty"`
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

	// Image for the Caddy proxy and maintenance containers.
	// +kubebuilder:default="caddy:2"
	// +optional
	Image string `json:"image,omitempty"`

	// Maintenance configures the fallback page served when the app is down.
	// +optional
	Maintenance MaintenanceSpec `json:"maintenance,omitempty"`

	// ClientIPHeader, when set, overrides X-Real-IP and X-Forwarded-For from the
	// named header (e.g. CF-Connecting-IP behind Cloudflare). When empty, the
	// upstream's X-Forwarded-For is preserved, trusting private-range upstreams
	// via Caddy trusted_proxies.
	// +optional
	ClientIPHeader string `json:"clientIPHeader,omitempty"`

	// Resources for the proxy container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MaintenanceSpec configures the maintenance fallback served on backend errors.
type MaintenanceSpec struct {
	// Enabled toggles the maintenance fallback Deployment.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// HTML is the page body served during maintenance. A default is used when empty.
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
	// +optional
	Host string `json:"host,omitempty"`

	// Annotations added to the Ingress object.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// TLSSecretName enables a TLS block referencing the named secret. Optional.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
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
type PostgresSpec struct {
	// External points Misskey at an existing PostgreSQL. When set, CNPG is not used.
	// +optional
	External *ExternalPostgres `json:"external,omitempty"`

	// Instances is the CNPG cluster size (1 = single, >=2 = HA with replicas).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// ImageName overrides the CNPG PostgreSQL image.
	// +kubebuilder:default="ghcr.io/cloudnative-pg/postgresql:17"
	// +optional
	ImageName string `json:"imageName,omitempty"`

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
}

// S3Credentials references S3-compatible credentials stored in secrets.
type S3Credentials struct {
	// AccessKeyID references the access key id.
	AccessKeyID corev1.SecretKeySelector `json:"accessKeyId"`
	// SecretAccessKey references the secret access key.
	SecretAccessKey corev1.SecretKeySelector `json:"secretAccessKey"`
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
	// +kubebuilder:validation:Enum=Progressing;Running;Error
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
