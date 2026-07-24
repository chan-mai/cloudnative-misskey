#!/usr/bin/env bash
# kind上でoperatorを実misskey imageと結合検証するe2e
# 前提: docker, kubectl。kind/kustomizeはmake test-e2eがbin/へ導入する
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=${E2E_CLUSTER:-cnm-e2e}
IMG=${IMG:-cloudnative-misskey:e2e}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.20.3}
CNPG_VERSION=${CNPG_VERSION:-1.30.0}
# 既定バージョンのマニフェストsha256。バージョン変更時は併せて更新するか空でskip
CERT_MANAGER_SHA256=${CERT_MANAGER_SHA256:-7ee74ba06845213e96d8ceaff3d20dd51e682765c1418eddda4e8780ba082261}
CNPG_SHA256=${CNPG_SHA256:-f8bede43fe4ee0d478c2355b204a36876b2ae4faac60f2a9452280b293da3b88}
KIND=bin/kind
KUSTOMIZE=bin/kustomize

# apply_verified URL EXPECTED_SHA256(空でskip): DL→sha256照合→server-side apply
apply_verified() {
  local url="$1" want="$2" tmp got
  tmp=$(mktemp)
  curl -sfL "$url" -o "$tmp"
  if [ -n "$want" ]; then
    got=$(shasum -a 256 "$tmp" | cut -d' ' -f1)
    if [ "$got" != "$want" ]; then
      echo "checksum mismatch for $url: got $got want $want" >&2
      rm -f "$tmp"
      exit 1
    fi
  fi
  kubectl apply --server-side -f "$tmp"
  rm -f "$tmp"
}

if ! $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  $KIND create cluster --name "$CLUSTER" --wait 120s
fi
kubectl config use-context "kind-$CLUSTER"

echo ">>> cert-manager ${CERT_MANAGER_VERSION}"
apply_verified "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml" "$CERT_MANAGER_SHA256"
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl -n cert-manager rollout status "deploy/$d" --timeout=180s
done

echo ">>> CloudNativePG ${CNPG_VERSION}"
apply_verified "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml" "$CNPG_SHA256"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s

echo ">>> operator ${IMG}"
docker build -t "$IMG" .
# 内容ハッシュでタグを決定し、コード変更時のみ自動ロール
SRC_HASH=$(find cmd api internal go.mod go.sum Dockerfile -type f -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256 | cut -c1-12)
ROLLOUT_IMG="${IMG}-${SRC_HASH}"
docker tag "$IMG" "$ROLLOUT_IMG"
$KIND load docker-image "$ROLLOUT_IMG" --name "$CLUSTER"
(cd config/manager && ../../$KUSTOMIZE edit set image "controller=$ROLLOUT_IMG")
$KUSTOMIZE build config/default-webhook | kubectl apply --server-side -f -
git checkout -- config/manager/kustomization.yaml
kubectl -n cloudnative-misskey-system rollout status deploy/controller-manager --timeout=300s
# ロール完了後、参照されなくなった旧タグをnodeから掃除(内容アドレスタグの堆積防止)
docker exec "${CLUSTER}-control-plane" crictl rmi --prune >/dev/null 2>&1 || true

echo ">>> e2e tests"
# -count=1: 実クラスタ相手のためgo testの結果キャッシュを無効化
go test -tags e2e -count=1 ./test/e2e/ -v -timeout 25m

if [ "${E2E_TEARDOWN:-0}" = "1" ]; then
  $KIND delete cluster --name "$CLUSTER"
fi
