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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// SetupMisskeyChannelWebhookWithManager: MisskeyChannelのvalidatorを登録
// cluster-scopedなChannelは全参照インスタンスへ任意imageを配信しうるため、許可リストで縛る
func SetupMisskeyChannelWebhookWithManager(mgr ctrl.Manager, allowedImageRegistries []string) error {
	return ctrl.NewWebhookManagedBy(mgr, &misskeyv1beta1.MisskeyChannel{}).
		WithValidator(&MisskeyChannelCustomValidator{AllowedImageRegistries: allowedImageRegistries}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-cloudnative-misskey-dev-v1beta1-misskeychannel,mutating=false,failurePolicy=fail,sideEffects=None,groups=cloudnative-misskey.dev,resources=misskeychannels,verbs=create;update,versions=v1beta1,name=vmisskeychannel-v1beta1.kb.io,admissionReviewVersions=v1

// MisskeyChannelCustomValidator: spec.imageの許可リスト強制
type MisskeyChannelCustomValidator struct {
	AllowedImageRegistries []string
}

var _ admission.Validator[*misskeyv1beta1.MisskeyChannel] = &MisskeyChannelCustomValidator{}

func (v *MisskeyChannelCustomValidator) ValidateCreate(_ context.Context, ch *misskeyv1beta1.MisskeyChannel) (admission.Warnings, error) {
	return nil, v.validate(ch)
}

func (v *MisskeyChannelCustomValidator) ValidateUpdate(_ context.Context, _, newObj *misskeyv1beta1.MisskeyChannel) (admission.Warnings, error) {
	return nil, v.validate(newObj)
}

func (v *MisskeyChannelCustomValidator) ValidateDelete(_ context.Context, _ *misskeyv1beta1.MisskeyChannel) (admission.Warnings, error) {
	return nil, nil
}

func (v *MisskeyChannelCustomValidator) validate(ch *misskeyv1beta1.MisskeyChannel) error {
	if imageAllowed(ch.Spec.Image, v.AllowedImageRegistries) {
		return nil
	}
	errs := field.ErrorList{field.Invalid(field.NewPath("spec", "image"), ch.Spec.Image,
		"image registry is not in the allowed list (--allowed-image-registries)")}
	return apierrors.NewInvalid(schema.GroupKind{Group: "cloudnative-misskey.dev", Kind: "MisskeyChannel"}, ch.Name, errs)
}
