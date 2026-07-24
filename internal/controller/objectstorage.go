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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	misskeyv1beta1 "github.com/chan-mai/cloudnative-misskey/api/v1beta1"
)

// metaRowID: Misskeyのmetaテーブルの固定主キー(単一行)
const metaRowID = "x"

// identRe: SQL識別子(カラム名)の許可文字。CELと同一。二重化して防御
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// safeIdent: カラム名がSQL識別子として妥当か検証(不正はerror)
func safeIdent(name string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("invalid SQL identifier %q", name)
	}
	return nil
}

// objAssign: metaの1カラムへの代入。値注入の安全化のため代入方法を型で分ける
//   - literal: operator決定の型付き値(bool/int/NULL)をSQL本文へ直書き(注入面なし)
//   - env: ユーザ文字列/秘密をenv経由で\getenv取り込み、:'psqlVar'でpsqlが安全quote
type objAssign struct {
	column  string // 解決済みカラム名(safeIdent済み)
	literal string // 非空ならSQL本文へ直書き(true/false/整数/NULL)
	envName string // 非空ならenv->\getenv->:'psqlVar'
	psqlVar string
	value   string                    // env(非秘密)の平文値
	secret  *corev1.SecretKeySelector // 非nilならenvはSecretKeyRef
}

// objectStorageAssignments: planからmetaカラム代入の順序付きリストを構築
// bool/portは型付きリテラル、文字列/秘密はenv経由。識別子はsafeIdentで再検証し衝突を弾く
func objectStorageAssignments(p plan) ([]objAssign, error) {
	col := func(logical string) string { return p.objColumns[logical] }
	used := map[string]bool{}
	var out []objAssign

	// literalカラム(operator決定・型付き=注入面なし)
	addLiteral := func(logical, lit string) error {
		c := col(logical)
		if err := safeIdent(c); err != nil {
			return err
		}
		used[c] = true
		out = append(out, objAssign{column: c, literal: lit})
		return nil
	}
	// env経由カラム(ユーザ文字列)。空文字はNULL(未設定)
	addString := func(logical, val string) error {
		c := col(logical)
		if err := safeIdent(c); err != nil {
			return err
		}
		used[c] = true
		if val == "" {
			out = append(out, objAssign{column: c, literal: "NULL"})
			return nil
		}
		out = append(out, objAssign{column: c, envName: "OBJVAL_" + logical, psqlVar: "v_" + logical, value: val})
		return nil
	}
	// env経由カラム(秘密)
	addSecret := func(logical string, sel corev1.SecretKeySelector) error {
		c := col(logical)
		if err := safeIdent(c); err != nil {
			return err
		}
		s := sel
		used[c] = true
		out = append(out, objAssign{column: c, envName: "OBJVAL_" + logical, psqlVar: "v_" + logical, secret: &s})
		return nil
	}

	if err := addLiteral("useObjectStorage", "true"); err != nil {
		return nil, err
	}
	// 決定的順序で追加(checksum安定のため)
	if err := addString("bucket", p.objBucket); err != nil {
		return nil, err
	}
	if err := addString("endpoint", p.objEndpoint); err != nil {
		return nil, err
	}
	if err := addString("region", p.objRegion); err != nil {
		return nil, err
	}
	if err := addString("prefix", p.objPrefix); err != nil {
		return nil, err
	}
	if err := addString("baseUrl", p.objBaseURL); err != nil {
		return nil, err
	}
	// port(*int32): nilはNULL、値は整数リテラル
	portLit := "NULL"
	if p.objPort != nil {
		portLit = strconv.Itoa(int(*p.objPort))
	}
	if err := addLiteral("port", portLit); err != nil {
		return nil, err
	}
	if err := addSecret("accessKey", p.objAccessKeySel); err != nil {
		return nil, err
	}
	if err := addSecret("secretKey", p.objSecretKeySel); err != nil {
		return nil, err
	}
	// bool群も決定的順序で
	for _, bl := range []struct {
		logical string
		b       bool
	}{
		{"useSSL", p.objUseSSL},
		{"useProxy", p.objUseProxy},
		{"setPublicRead", p.objSetPublicRead},
		{"s3ForcePathStyle", p.objForcePathStyle},
	} {
		if err := addLiteral(bl.logical, strconv.FormatBool(bl.b)); err != nil {
			return nil, err
		}
	}

	// extraColumns(fork固有・平文)。キーはsafeIdent、標準カラムとの衝突を弾く
	keys := make([]string, 0, len(p.objExtra))
	for k := range p.objExtra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if err := safeIdent(k); err != nil {
			return nil, err
		}
		if used[k] {
			return nil, fmt.Errorf("extraColumns key %q collides with a standard object storage column", k)
		}
		used[k] = true
		out = append(out, objAssign{
			column:  k,
			envName: fmt.Sprintf("OBJEXTRA_%d", i),
			psqlVar: fmt.Sprintf("x_%d", i),
			value:   p.objExtra[k],
		})
	}
	return out, nil
}

// renderObjectStorageSQL: meta行を保証(INSERT ON CONFLICT)してからUPDATEする単一トランザクションSQL
// 値はSQL本文に連結せず\getenv+:'var'で安全quote、識別子はsafeIdent済みをダブルクォート
func renderObjectStorageSQL(assigns []objAssign) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("-- Managed by cloudnative-misskey. Do not edit by hand.\n")
	w("\\set ON_ERROR_STOP on\n")
	for _, a := range assigns {
		if a.envName != "" {
			w("\\getenv %s %s\n", a.psqlVar, a.envName)
		}
	}
	w("INSERT INTO meta (id) VALUES ('%s') ON CONFLICT (id) DO NOTHING;\n", metaRowID)
	w("UPDATE meta SET\n")
	for i, a := range assigns {
		sep := ","
		if i == len(assigns)-1 {
			sep = ""
		}
		if a.envName != "" {
			w("  %q = :'%s'%s\n", a.column, a.psqlVar, sep)
		} else {
			w("  %q = %s%s\n", a.column, a.literal, sep)
		}
	}
	w("WHERE id = '%s';\n", metaRowID)
	return b.String()
}

// objectStorageJobEnv: 代入リストからJob env(平文Value / SecretKeyRef)を構築
func objectStorageJobEnv(assigns []objAssign) []corev1.EnvVar {
	var env []corev1.EnvVar
	for _, a := range assigns {
		if a.envName == "" {
			continue
		}
		if a.secret != nil {
			env = append(env, secretEnv(a.envName, *a.secret))
		} else {
			env = append(env, corev1.EnvVar{Name: a.envName, Value: a.value})
		}
	}
	return env
}

// objectStorageHash: 入力のsha256。Job名(先頭10hex)に使い、入力変化で別Job・旧掃除
// SQL本文(カラム名/bool/port/NULL構造)+ env平文値 + 資格情報secretのresourceVersion
// (SQL本文に文字列値は出ないため値変化はここで捕捉。credentialローテも再投入対象)
func (r *MisskeyReconciler) objectStorageHash(ctx context.Context, m *misskeyv1beta1.Misskey, p plan, sql string, assigns []objAssign) string {
	h := sha256.New()
	writePart := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	writePart(sql)
	for _, a := range assigns {
		if a.envName != "" && a.secret == nil {
			writePart(a.column + "=" + a.value)
		}
	}
	for _, v := range r.credentialSecretVersions(ctx, m, p) {
		writePart(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// credentialSecretVersions: objectStorage資格情報secretのname:resourceVersion(dedup・sort)
func (r *MisskeyReconciler) credentialSecretVersions(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) []string {
	names := map[string]bool{p.objAccessKeySel.Name: true, p.objSecretKeySel.Name: true}
	out := make([]string, 0, len(names))
	for name := range names {
		version := "missing"
		s := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: m.Namespace}, s); err == nil {
			version = s.ResourceVersion
		}
		out = append(out, name+":"+version)
	}
	sort.Strings(out)
	return out
}

// reconcileObjectStorage: meta書込Jobを望ましい状態へ収束。完了(Succeeded>=1)を返す
// 入力変化はJob名(hash)変化で捕捉し旧Jobを掃除。呼び出し側はp.objAutoConfigure時のみ呼ぶ
func (r *MisskeyReconciler) reconcileObjectStorage(ctx context.Context, m *misskeyv1beta1.Misskey, p plan) (bool, error) {
	assigns, err := objectStorageAssignments(p)
	if err != nil {
		return false, err
	}
	sql := renderObjectStorageSQL(assigns)
	hash := r.objectStorageHash(ctx, m, p, sql, assigns)
	jobName := nameObjectStorage(m, hash)

	// 入力変化で名前が変わるため、現行名以外のobjstorage Jobを掃除
	if err := r.cleanupObjectStorageJobs(ctx, m, jobName); err != nil {
		return false, err
	}

	// SQL ConfigMap(stable名)をupsert
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameObjectStorageSQL(m), Namespace: m.Namespace}}
	if err := r.apply(ctx, m, cm, func() error {
		cm.Labels = labelsFor(m, "objstorage")
		cm.Data = map[string]string{"objectstorage.sql": sql}
		return nil
	}); err != nil {
		return false, err
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: m.Namespace}, job)
	if apierrors.IsNotFound(err) {
		job = buildObjectStorageJob(m, p, jobName, objectStorageJobEnv(assigns))
		if err := controllerutil.SetControllerReference(m, job, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, job); err != nil {
			return false, err
		}
		r.event(m, corev1.EventTypeNormal, "ObjectStorageConfiguring", "Configure", "created object storage meta Job %s", job.Name)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if job.Status.Succeeded == 0 && job.Status.Failed >= 1 {
		r.event(m, corev1.EventTypeWarning, "ObjectStorageFailed", "Configure", "object storage meta Job %s failed (%d); delete the Job to retry", job.Name, job.Status.Failed)
	}
	return job.Status.Succeeded >= 1, nil
}

// cleanupObjectStorageJobs: keep以外のobjstorage Jobを削除。keep=""で全削除(無効化cleanup)
func (r *MisskeyReconciler) cleanupObjectStorageJobs(ctx context.Context, m *misskeyv1beta1.Misskey, keep string) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels(selectorFor(m, "objstorage"))); err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Name == keep {
			continue
		}
		if err := r.Delete(ctx, j, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// cleanupObjectStorage: objectStorage無効化/autoConfigure=false時のJob+SQL ConfigMap掃除
// metaは触らない(useObjectStorage=falseは既存S3ファイル配信を壊す破壊的操作のため)
func (r *MisskeyReconciler) cleanupObjectStorage(ctx context.Context, m *misskeyv1beta1.Misskey) error {
	if err := r.cleanupObjectStorageJobs(ctx, m, ""); err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nameObjectStorageSQL(m), Namespace: m.Namespace}}
	return r.deleteIfExists(ctx, cm)
}

// buildObjectStorageJob: psqlでmetaへ設定を書き込む使い捨てJob
func buildObjectStorageJob(m *misskeyv1beta1.Misskey, p plan, name string, objEnv []corev1.EnvVar) *batchv1.Job {
	// 書込は必ずprimaryへ(pooler/replica迂回)。migratePlanを再利用
	mp := migratePlan(m, p)
	env := []corev1.EnvVar{
		{Name: "PGHOST", Value: mp.dbHost},
		{Name: "PGPORT", Value: strconv.Itoa(int(mp.dbPort))},
		{Name: "PGDATABASE", Value: mp.dbName},
		{Name: "PGUSER", Value: mp.dbUser},
		secretEnv("PGPASSWORD", mp.dbPassSel),
		{Name: "HOME", Value: "/tmp"}, // psql履歴等の書込先
	}
	env = append(env, objEnv...)

	pod := corev1.PodSpec{
		AutomountServiceAccountToken: boolPtr(false),
		RestartPolicy:                corev1.RestartPolicyOnFailure,
		ImagePullSecrets:             m.Spec.ImagePullSecrets,
		SecurityContext:              nonRootPodSecurityContext(genericNonRootUID),
		Containers: []corev1.Container{{
			Name:            "objectstorage",
			Image:           p.objImage,
			Command:         []string{"psql", "-1", "-f", "/sql/objectstorage.sql"},
			Env:             env,
			SecurityContext: restrictedContainerSecurityContext(),
			Resources:       resourcesOr(corev1.ResourceRequirements{}, "50m", "64Mi", "128Mi"),
			VolumeMounts:    []corev1.VolumeMount{{Name: "sql", MountPath: "/sql", ReadOnly: true}, tmpMount()},
		}},
		Volumes: []corev1.Volume{
			{
				Name: "sql",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: nameObjectStorageSQL(m)},
					},
				},
			},
			tmpVolume(),
		},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace, Labels: labelsFor(m, "objstorage")},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(20), // DB起動待ちの猶予
			Parallelism:  int32Ptr(1),
			Completions:  int32Ptr(1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(m, "objstorage")},
				Spec:       pod,
			},
		},
	}
}
