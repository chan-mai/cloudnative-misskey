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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// immutable(url/id/tenant)やcross-field整合(external xor managed、min<=max等)は
// CRDのCEL(XValidation)が常時強制するため、ここではwebhook固有の
// defaulter/警告のみをテストする。CELルールはintegration_test.go(envtest)が検証する。

func base() *misskeyv1beta1.Misskey {
	return &misskeyv1beta1.Misskey{
		ObjectMeta: metav1.ObjectMeta{Name: "ex", Namespace: "ns"},
		Spec:       misskeyv1beta1.MisskeySpec{URL: "https://m.example.com/", Image: "misskey/misskey:x"},
	}
}

func TestDefaultTenant(t *testing.T) {
	d := &MisskeyCustomDefaulter{}
	m := base()
	if err := d.Default(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Tenant != "ns" {
		t.Errorf("tenant default: got %q, want ns", m.Spec.Tenant)
	}
	// 設定済みは上書きしない
	m2 := base()
	m2.Spec.Tenant = "acme"
	_ = d.Default(context.Background(), m2)
	if m2.Spec.Tenant != "acme" {
		t.Errorf("tenant overwritten: %q", m2.Spec.Tenant)
	}
}

func TestValidateCreateOK(t *testing.T) {
	v := &MisskeyCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), base()); err != nil {
		t.Errorf("valid spec rejected: %v", err)
	}
}

func TestValidateImageAllowlist(t *testing.T) {
	v := &MisskeyCustomValidator{AllowedImageRegistries: []string{"ghcr.io/trusted/"}}
	// 許可外imageは拒否
	m := base()
	m.Spec.Image = "misskey/misskey:x"
	if _, err := v.ValidateCreate(context.Background(), m); err == nil {
		t.Error("image outside allowlist must be rejected")
	}
	// 許可prefixに一致すれば通る
	m.Spec.Image = "ghcr.io/trusted/misskey:x"
	if _, err := v.ValidateCreate(context.Background(), m); err != nil {
		t.Errorf("allowed image rejected: %v", err)
	}
	// 空allowlistは全許可
	if _, err := (&MisskeyCustomValidator{}).ValidateCreate(context.Background(), base()); err != nil {
		t.Errorf("empty allowlist must allow any: %v", err)
	}
	// spec.image以外(proxy/postgres/redis HA/meili等)も許可リスト対象
	for _, tc := range []func(*misskeyv1beta1.Misskey){
		func(x *misskeyv1beta1.Misskey) { x.Spec.Proxy.Image = "evil/caddy:2" },
		func(x *misskeyv1beta1.Misskey) { x.Spec.Postgres.Image = "evil/pg:16" },
		func(x *misskeyv1beta1.Misskey) { x.Spec.Redis.Image = "evil/redis:8" },
		func(x *misskeyv1beta1.Misskey) {
			x.Spec.Redis.HA = &misskeyv1beta1.RedisHA{Image: "evil/redis:8"}
		},
		func(x *misskeyv1beta1.Misskey) { x.Spec.Search.Meilisearch.Image = "evil/meili:v1" },
	} {
		mm := base()
		mm.Spec.Image = "ghcr.io/trusted/misskey:x"
		tc(mm)
		if _, err := v.ValidateCreate(context.Background(), mm); err == nil {
			t.Error("non-spec.image field outside allowlist must be rejected")
		}
	}
}

func TestValidateClusterIssuerAllowlist(t *testing.T) {
	v := &MisskeyCustomValidator{AllowedClusterIssuers: []string{"letsencrypt"}}
	m := base()
	m.Spec.Ingress.IssuerRef = &misskeyv1beta1.IngressIssuerRef{Name: "rogue"}
	if _, err := v.ValidateCreate(context.Background(), m); err == nil {
		t.Error("disallowed ClusterIssuer must be rejected")
	}
	// namespaced Issuerは許可リスト対象外
	m.Spec.Ingress.IssuerRef = &misskeyv1beta1.IngressIssuerRef{Name: "rogue", Kind: "Issuer"}
	if _, err := v.ValidateCreate(context.Background(), m); err != nil {
		t.Errorf("namespaced Issuer must not be gated: %v", err)
	}
	// 許可名は通る
	m.Spec.Ingress.IssuerRef = &misskeyv1beta1.IngressIssuerRef{Name: "letsencrypt"}
	if _, err := v.ValidateCreate(context.Background(), m); err != nil {
		t.Errorf("allowed ClusterIssuer rejected: %v", err)
	}
}

func TestValidateChannelImageAllowlist(t *testing.T) {
	v := &MisskeyChannelCustomValidator{AllowedImageRegistries: []string{"ghcr.io/trusted/"}}
	ch := &misskeyv1beta1.MisskeyChannel{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec:       misskeyv1beta1.MisskeyChannelSpec{Image: "evil/img:latest"},
	}
	if _, err := v.ValidateCreate(context.Background(), ch); err == nil {
		t.Error("channel image outside allowlist must be rejected")
	}
	ch.Spec.Image = "ghcr.io/trusted/misskey:1"
	if _, err := v.ValidateCreate(context.Background(), ch); err != nil {
		t.Errorf("allowed channel image rejected: %v", err)
	}
}

func TestAdvisoryWarnings(t *testing.T) {
	// external DB + readOffload → 効かない旨の警告(エラーではない)
	m := base()
	on := true
	m.Spec.Postgres.External = &misskeyv1beta1.ExternalPostgres{Host: "h", Database: "d", User: "u"}
	m.Spec.Postgres.ReadOffload = &on
	warns := advisoryWarnings(m)
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "readOffload") {
		t.Errorf("expected readOffload advisory warning, got %v", warns)
	}
	// recovery → 復元元との整合(url/idGenerationMethod/imageName)注意の警告
	m2 := base()
	m2.Spec.Postgres.Recovery = &misskeyv1beta1.PostgresRecovery{Source: misskeyv1beta1.RecoverySource{
		DestinationPath: "s3://bk/misskey", ServerName: "old-db",
	}}
	warns = advisoryWarnings(m2)
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "recovery") {
		t.Errorf("expected recovery advisory warning, got %v", warns)
	}
	// rps + monitoring無効 → 警告
	m3 := base()
	m3.Spec.App.Autoscaling = &misskeyv1beta1.AppAutoscalingSpec{
		AutoscalingSpec: misskeyv1beta1.AutoscalingSpec{MaxReplicas: 3},
		RPS:             &misskeyv1beta1.RPSTrigger{ServerAddress: "http://prom:9090", TargetRPS: 50},
	}
	warns = advisoryWarnings(m3)
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, " "), "monitoring.enabled") {
		t.Errorf("expected rps monitoring warning, got %v", warns)
	}
	m3.Spec.Monitoring.Enabled = &on
	if w := advisoryWarnings(m3); len(w) != 0 {
		t.Errorf("rps with monitoring must not warn: %v", w)
	}

	// mutableなlatestタグ+digest追従なし → 警告
	for _, img := range []string{"misskey/misskey:latest", "misskey/misskey"} {
		m4 := base()
		m4.Spec.Image = img
		if w := advisoryWarnings(m4); len(w) == 0 || !strings.Contains(strings.Join(w, " "), "trackImageDigest") {
			t.Errorf("expected latest-tag warning for %q, got %v", img, w)
		}
		m4.Spec.TrackImageDigest = true
		if w := advisoryWarnings(m4); len(w) != 0 {
			t.Errorf("trackImageDigest enabled must not warn: %v", w)
		}
	}

	// 正常specは警告なし
	if w := advisoryWarnings(base()); len(w) != 0 {
		t.Errorf("clean spec must have no warnings: %v", w)
	}
}

func TestExtraConfigWarnings(t *testing.T) {
	if w := extraConfigWarnings(""); w != nil {
		t.Errorf("empty extraConfig must not warn: %v", w)
	}
	if w := extraConfigWarnings("cacheServer:\n  host: redis2\n"); w != nil {
		t.Errorf("non-reserved keys must not warn: %v", w)
	}
	// YAML破損
	if w := extraConfigWarnings(": bad ["); len(w) != 1 || !strings.Contains(w[0], "not valid YAML") {
		t.Errorf("broken YAML must warn: %v", w)
	}
	// 予約キー衝突(複数, ソート順)
	w := extraConfigWarnings("redis:\n  host: x\ndb:\n  host: y\n")
	if len(w) != 2 || !strings.Contains(w[0], `"db"`) || !strings.Contains(w[1], `"redis"`) {
		t.Errorf("reserved key conflicts must warn sorted: %v", w)
	}
}
