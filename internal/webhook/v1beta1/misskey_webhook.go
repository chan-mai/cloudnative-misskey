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
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// imageAllowed: allowed空なら全許可、非空ならいずれかのprefix一致で許可
func imageAllowed(image string, allowed []string) bool {
	if image == "" || len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.HasPrefix(image, a) {
			return true
		}
	}
	return false
}

// nameAllowed: allowed空なら全許可、非空なら完全一致で許可
func nameAllowed(name string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if name == a {
			return true
		}
	}
	return false
}

// operatorがdefault.ymlへ出力するトップレベルキー(controller側renderDefaultYMLと同期)
// extraConfigとの重複はjs-yamlのduplicated mapping keyエラーでMisskeyが起動しなくなる
var reservedConfigKeys = map[string]bool{
	"url": true, "port": true, "setupPassword": true,
	"db": true, "dbReplications": true, "dbSlaves": true,
	"redis": true, "redisForJobQueue": true, "redisForPubsub": true,
	"redisForTimelines": true, "redisForReactions": true,
	"fulltextSearch": true, "meilisearch": true,
	"id": true, "proxyRemoteFiles": true, "signToActivityPubGet": true,
	"deliverJobConcurrency": true, "inboxJobConcurrency": true,
	"deliverJobPerSec": true, "inboxJobPerSec": true, "relationshipJobPerSec": true,
	"deliverJobMaxAttempts": true, "inboxJobMaxAttempts": true,
	"proxy": true, "proxySmtp": true, "proxyBypassHosts": true,
	"maxFileSize": true, "mediaProxy": true,
}

// SetupMisskeyWebhookWithManager: Misskeyのdefaulter/validatorをmanagerへ登録
//
// 不変性(url/idGenerationMethod/tenant)やcross-field整合(external xor managed、
// pooler/backupのmanaged必須、autoscaling min<=max、role排他)はCRDのCEL
// (XValidation)で常時強制しており、webhook未導入でも効きます。webhookはCELで
// 表せない項目だけを担当します: tenant未設定→namespaceのdefault(「未設定→初回設定」の
// 穴を塞ぐ)と、エラーにするほどでない補助的な警告です。
func SetupMisskeyWebhookWithManager(mgr ctrl.Manager, allowedImageRegistries, allowedClusterIssuers []string) error {
	return ctrl.NewWebhookManagedBy(mgr, &misskeyv1beta1.Misskey{}).
		WithDefaulter(&MisskeyCustomDefaulter{}).
		WithValidator(&MisskeyCustomValidator{
			AllowedImageRegistries: allowedImageRegistries,
			AllowedClusterIssuers:  allowedClusterIssuers,
		}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-cloudnative-misskey-dev-v1beta1-misskey,mutating=true,failurePolicy=fail,sideEffects=None,groups=cloudnative-misskey.dev,resources=misskeys,verbs=create;update,versions=v1beta1,name=mmisskey-v1beta1.kb.io,admissionReviewVersions=v1

// MisskeyCustomDefaulter: CRD既定で表せない項目を補完
type MisskeyCustomDefaulter struct{}

var _ admission.Defaulter[*misskeyv1beta1.Misskey] = &MisskeyCustomDefaulter{}

// Default: tenant未設定はnamespaceで確定。以後CELでimmutableとなり「未設定→初回設定」の穴を塞ぐ
func (d *MisskeyCustomDefaulter) Default(_ context.Context, m *misskeyv1beta1.Misskey) error {
	if m.Spec.Tenant == "" {
		m.Spec.Tenant = m.Namespace
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-cloudnative-misskey-dev-v1beta1-misskey,mutating=false,failurePolicy=fail,sideEffects=None,groups=cloudnative-misskey.dev,resources=misskeys,verbs=create;update,versions=v1beta1,name=vmisskey-v1beta1.kb.io,admissionReviewVersions=v1

// MisskeyCustomValidator: CELで表せない補助的な警告 + flag由来の許可リスト強制(拒否)
// 許可リストは運用者指定でCELに埋め込めないためwebhook層で担当する
type MisskeyCustomValidator struct {
	AllowedImageRegistries []string
	AllowedClusterIssuers  []string
}

var _ admission.Validator[*misskeyv1beta1.Misskey] = &MisskeyCustomValidator{}

func (v *MisskeyCustomValidator) ValidateCreate(_ context.Context, m *misskeyv1beta1.Misskey) (admission.Warnings, error) {
	if err := v.validate(m); err != nil {
		return nil, err
	}
	return advisoryWarnings(m), nil
}

func (v *MisskeyCustomValidator) ValidateUpdate(_ context.Context, _, newObj *misskeyv1beta1.Misskey) (admission.Warnings, error) {
	if err := v.validate(newObj); err != nil {
		return nil, err
	}
	return advisoryWarnings(newObj), nil
}

func (v *MisskeyCustomValidator) ValidateDelete(_ context.Context, _ *misskeyv1beta1.Misskey) (admission.Warnings, error) {
	return nil, nil
}

// specImages: CRから指定できる全imageを fieldPath→image で返す(空は除外)
// レンダリング前の共通検証で、許可リストを全image入力へ適用するため
func specImages(m *misskeyv1beta1.Misskey) map[string]string {
	out := map[string]string{}
	add := func(path, img string) {
		if img != "" {
			out[path] = img
		}
	}
	add("spec.image", m.Spec.Image)
	add("spec.proxy.image", m.Spec.Proxy.Image)
	add("spec.postgres.image", m.Spec.Postgres.Image)
	add("spec.redis.image", m.Spec.Redis.Image)
	if ha := m.Spec.Redis.HA; ha != nil {
		add("spec.redis.ha.image", ha.Image)
		add("spec.redis.ha.sentinelImage", ha.SentinelImage)
	}
	if roles := m.Spec.Redis.Roles; roles != nil {
		for name, role := range map[string]*misskeyv1beta1.RedisRole{
			"jobQueue": roles.JobQueue, "pubsub": roles.Pubsub,
			"timelines": roles.Timelines, "reactions": roles.Reactions,
		} {
			if role != nil && role.HA != nil {
				add("spec.redis.roles."+name+".ha.image", role.HA.Image)
				add("spec.redis.roles."+name+".ha.sentinelImage", role.HA.SentinelImage)
			}
		}
	}
	add("spec.search.meilisearch.image", m.Spec.Search.Meilisearch.Image)
	if os := m.Spec.ObjectStorage; os != nil {
		add("spec.objectStorage.image", os.Image)
	}
	add("spec.monitoring.redisExporterImage", m.Spec.Monitoring.RedisExporterImage)
	return out
}

// validate: 許可リスト違反をfield errorで拒否
func (v *MisskeyCustomValidator) validate(m *misskeyv1beta1.Misskey) error {
	var errs field.ErrorList
	for path, img := range specImages(m) {
		if !imageAllowed(img, v.AllowedImageRegistries) {
			errs = append(errs, field.Invalid(field.NewPath(path), img,
				"image registry is not in the allowed list (--allowed-image-registries)"))
		}
	}
	// ClusterIssuer参照のみ許可リスト適用(namespaced Issuerは同一namespace内で対象外)
	if ref := m.Spec.Ingress.IssuerRef; ref != nil && ref.Kind != "Issuer" && !nameAllowed(ref.Name, v.AllowedClusterIssuers) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "ingress", "issuerRef", "name"), ref.Name,
			"ClusterIssuer is not in the allowed list (--allowed-cluster-issuers)"))
	}
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: "cloudnative-misskey.dev", Kind: "Misskey"}, m.Name, errs)
}

// advisoryWarnings: エラーにはしないが利用者に気づかせたい設定を警告として返す
// (無効値のブロックはCELが拒否するため、ここは「設定はできるが効かない」系のみ)
func advisoryWarnings(m *misskeyv1beta1.Misskey) admission.Warnings {
	var warns admission.Warnings
	pg := m.Spec.Postgres
	if pg.External != nil {
		if pg.ReadOffload != nil && *pg.ReadOffload {
			warns = append(warns, "spec.postgres.readOffload has no effect with an external database")
		}
	} else if pg.ReadOffload != nil && *pg.ReadOffload && instancesOr(pg.Instances) < 2 {
		warns = append(warns, "spec.postgres.readOffload needs postgres.instances>=2 to take effect")
	}
	if m.Spec.Redis.External != nil && m.Spec.Redis.Roles != nil {
		warns = append(warns, "spec.redis.roles is ignored while redis.external is set")
	}
	if m.Spec.Search.Provider == misskeyv1beta1.SearchSQLPgroonga && pg.External == nil && pg.Image == "" {
		warns = append(warns, "search.provider=sqlPgroonga requires postgres.image with the PGroonga extension")
	}
	if pg.Recovery != nil || pg.Import != nil {
		warns = append(warns, "spec.postgres.recovery/import restores an existing database: keep spec.url and spec.idGenerationMethod identical to the source instance, and use a postgres.image compatible with the source's PostgreSQL major version and installed extensions")
	}
	rpsSet := m.Spec.App.Autoscaling != nil && m.Spec.App.Autoscaling.RPS != nil
	if rpsSet && (m.Spec.Monitoring.Enabled == nil || !*m.Spec.Monitoring.Enabled) {
		warns = append(warns, "autoscaling.rps needs monitoring.enabled so the proxy metrics port is exposed and scraped")
	}
	if img := m.Spec.Image; img != "" && !m.Spec.TrackImageDigest && !strings.Contains(img, "@") {
		// タグ無し(=latest)か明示:latestはmutable。digest追従なしだと再pushでrollしない
		if !strings.Contains(img[strings.LastIndex(img, "/")+1:], ":") || strings.HasSuffix(img, ":latest") {
			warns = append(warns, "spec.image uses a mutable latest tag; set trackImageDigest: true or pin a version/digest, otherwise new pushes never roll app/worker")
		}
	}
	if os := m.Spec.ObjectStorage; os != nil {
		if os.SetPublicRead != nil && *os.SetPublicRead && strings.Contains(os.Endpoint, "r2.cloudflarestorage.com") {
			warns = append(warns, "spec.objectStorage.setPublicRead must be false for Cloudflare R2 (it does not support object ACLs)")
		}
	}
	// import接続のsslModeが弱いと中間者による平文降格・傍受の余地
	if imp := pg.Import; imp != nil {
		switch imp.Source.SSLMode {
		case "disable", "allow", "prefer", "":
			warns = append(warns, "spec.postgres.import.source.sslMode is weaker than require; use require or verify-full to prevent a man-in-the-middle downgrade of the import connection")
		}
	}
	// http URLは連合先へ平文で公開される
	if strings.HasPrefix(m.Spec.URL, "http://") {
		warns = append(warns, "spec.url uses http://; federated peers will see a plaintext URL. Prefer https://")
	}
	warns = append(warns, extraConfigWarnings(m.Spec.ExtraConfig)...)
	return warns
}

// extraConfigWarnings: extraConfigのYAML破損とoperator管理キーとの重複を警告
func extraConfigWarnings(extra string) admission.Warnings {
	if strings.TrimSpace(extra) == "" {
		return nil
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(extra), &parsed); err != nil {
		return admission.Warnings{"spec.extraConfig is not valid YAML and will break Misskey's config parse"}
	}
	var conflicts []string
	for k := range parsed {
		if reservedConfigKeys[k] {
			conflicts = append(conflicts, k)
		}
	}
	sort.Strings(conflicts)
	var warns admission.Warnings
	for _, k := range conflicts {
		warns = append(warns, fmt.Sprintf("spec.extraConfig key %q duplicates an operator-managed key; duplicate top-level keys break Misskey's YAML parse", k))
	}
	return warns
}

// instancesOr: pg.Instances(0は既定1)
func instancesOr(instances int32) int32 {
	if instances == 0 {
		return 1
	}
	return instances
}
