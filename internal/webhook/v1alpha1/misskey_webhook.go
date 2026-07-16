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
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	misskeyv1alpha1 "github.com/chan-mai/cloudnative-misskey/api/v1alpha1"
)

// SetupMisskeyWebhookWithManager: Misskeyのdefaulter/validatorをmanagerへ登録
//
// 不変性(url/idGenerationMethod/tenant)やcross-field整合(external xor managed、
// pooler/backupのmanaged必須、autoscaling min<=max、role排他)はCRDのCEL
// (XValidation)で常時強制しており、webhook未導入でも効きます。webhookはCELで
// 表せない項目だけを担当します: tenant未設定→namespaceのdefault(「未設定→初回設定」の
// 穴を塞ぐ)と、エラーにするほどでない補助的な警告です。
func SetupMisskeyWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &misskeyv1alpha1.Misskey{}).
		WithDefaulter(&MisskeyCustomDefaulter{}).
		WithValidator(&MisskeyCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-cloudnative-misskey-dev-v1alpha1-misskey,mutating=true,failurePolicy=fail,sideEffects=None,groups=cloudnative-misskey.dev,resources=misskeys,verbs=create;update,versions=v1alpha1,name=mmisskey-v1alpha1.kb.io,admissionReviewVersions=v1

// MisskeyCustomDefaulter: CRD既定で表せない項目を補完
type MisskeyCustomDefaulter struct{}

var _ admission.Defaulter[*misskeyv1alpha1.Misskey] = &MisskeyCustomDefaulter{}

// Default: tenant未設定はnamespaceで確定。以後CELでimmutableとなり「未設定→初回設定」の穴を塞ぐ
func (d *MisskeyCustomDefaulter) Default(_ context.Context, m *misskeyv1alpha1.Misskey) error {
	if m.Spec.Tenant == "" {
		m.Spec.Tenant = m.Namespace
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-cloudnative-misskey-dev-v1alpha1-misskey,mutating=false,failurePolicy=fail,sideEffects=None,groups=cloudnative-misskey.dev,resources=misskeys,verbs=create;update,versions=v1alpha1,name=vmisskey-v1alpha1.kb.io,admissionReviewVersions=v1

// MisskeyCustomValidator: CELで表せない補助的な警告のみ(エラーはCELが常時強制)
type MisskeyCustomValidator struct{}

var _ admission.Validator[*misskeyv1alpha1.Misskey] = &MisskeyCustomValidator{}

func (v *MisskeyCustomValidator) ValidateCreate(_ context.Context, m *misskeyv1alpha1.Misskey) (admission.Warnings, error) {
	return advisoryWarnings(m), nil
}

func (v *MisskeyCustomValidator) ValidateUpdate(_ context.Context, _, newObj *misskeyv1alpha1.Misskey) (admission.Warnings, error) {
	return advisoryWarnings(newObj), nil
}

func (v *MisskeyCustomValidator) ValidateDelete(_ context.Context, _ *misskeyv1alpha1.Misskey) (admission.Warnings, error) {
	return nil, nil
}

// advisoryWarnings: エラーにはしないが利用者に気づかせたい設定を警告として返す
// (無効値のブロックはCELが拒否するため、ここは「設定はできるが効かない」系のみ)
func advisoryWarnings(m *misskeyv1alpha1.Misskey) admission.Warnings {
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
	if m.Spec.Search.Provider == misskeyv1alpha1.SearchSQLPgroonga && pg.External == nil && pg.ImageName == "" {
		warns = append(warns, "search.provider=sqlPgroonga requires postgres.imageName with the PGroonga extension")
	}
	if pg.Recovery != nil {
		warns = append(warns, "spec.postgres.recovery restores an existing database: keep spec.url and spec.idGenerationMethod identical to the source instance, and use a postgres.imageName compatible with the source's PostgreSQL major version and installed extensions")
	}
	if os := m.Spec.ObjectStorage; os != nil {
		if os.SetPublicRead != nil && *os.SetPublicRead && strings.Contains(os.Endpoint, "r2.cloudflarestorage.com") {
			warns = append(warns, "spec.objectStorage.setPublicRead must be false for Cloudflare R2 (it does not support object ACLs)")
		}
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
