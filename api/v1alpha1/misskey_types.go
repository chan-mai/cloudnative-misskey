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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MisskeySpec defines the desired state of a Misskey instance.
type MisskeySpec struct {
	// URL is the public-facing URL of the instance, e.g. https://misskey.example.com/.
	// Do not change this after the instance has been initialized.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+`
	URL string `json:"url"`

	// Image is the Misskey server container image. The app and worker share it.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// IDGenerationMethod is Misskey's note/user id format. Immutable after the
	// instance has been initialized; when migrating an existing database, set it
	// to the value the database was created with.
	// +kubebuilder:validation:Enum=aid;aidx;meid;ulid;objectid
	// +kubebuilder:default=aidx
	// +optional
	IDGenerationMethod string `json:"idGenerationMethod,omitempty"`

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

	// NetworkIsolation configures ingress isolation for this instance's backend
	// pods (app/worker/redis/meilisearch).
	// +optional
	NetworkIsolation NetworkIsolationSpec `json:"networkIsolation,omitempty"`

	// EgressIsolation configures egress isolation (opt-in). See EgressIsolationSpec.
	// +optional
	EgressIsolation EgressIsolationSpec `json:"egressIsolation,omitempty"`

	// Tenancy configures namespace-scoped isolation, assuming the namespace is
	// dedicated to this instance.
	// +optional
	Tenancy TenancySpec `json:"tenancy,omitempty"`
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
type RedisSpec struct {
	// External points Misskey at an existing Redis. When set, no Redis is managed.
	// +optional
	External *ExternalRedis `json:"external,omitempty"`

	// Image for the managed Redis StatefulSet.
	// +kubebuilder:default="redis:7-alpine"
	// +optional
	Image string `json:"image,omitempty"`

	// MaxMemory passes --maxmemory to redis-server.
	// +kubebuilder:default="400mb"
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

	// NetworkPolicy toggles a NetworkPolicy that limits ingress to the managed
	// Redis to app and worker pods. Default true. Only effective on CNIs that
	// enforce NetworkPolicy.
	// +kubebuilder:default=true
	// +optional
	NetworkPolicy *bool `json:"networkPolicy,omitempty"`

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
}

// ExternalRedis references a Redis running outside the operator's control.
type ExternalRedis struct {
	// Host of the external Redis.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port of the external Redis.
	// +kubebuilder:default=6379
	// +optional
	Port int32 `json:"port,omitempty"`

	// PasswordSecret optionally references the Redis password.
	// +optional
	PasswordSecret *corev1.SecretKeySelector `json:"passwordSecret,omitempty"`
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
	// +kubebuilder:default="getmeili/meilisearch:v1.11"
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

	// Schedule is a 6-field cron for a CNPG ScheduledBackup. Empty disables it.
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

	// Phase is a coarse, human-readable state summary.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mk
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Search",type=string,JSONPath=`.spec.search.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
