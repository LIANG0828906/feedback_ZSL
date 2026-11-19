#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/env.sh"

MODE="${1:-}"

reset_git_baseline() {
  local dev_base="refs/tags/drift-baseline-$DEV_BRANCH"
  local prod_base="refs/tags/drift-baseline-$PROD_BRANCH"
  git --git-dir="$REPO_DIR" show-ref --verify --quiet "$dev_base"
  git --git-dir="$REPO_DIR" show-ref --verify --quiet "$prod_base"
  git --git-dir="$REPO_DIR" update-ref "refs/heads/$DEV_BRANCH" "$dev_base"
  git --git-dir="$REPO_DIR" update-ref "refs/heads/$PROD_BRANCH" "$prod_base"
}

reset_cluster_baseline() {
  local role namespace
  for role in dev prod; do
    if [[ "$role" == "dev" ]]; then
      namespace="$DEV_NAMESPACE"
      extract_branch "$DEV_BRANCH" "$LAB_DIR/apply-dev"
    else
      namespace="$PROD_NAMESPACE"
      extract_branch "$PROD_BRANCH" "$LAB_DIR/apply-prod"
    fi
    kubectl_local -n "$namespace" delete deployment checkout-api worker --ignore-not-found=true >/dev/null 2>&1 || true
    apply_snapshot_manifests "$LAB_DIR/apply-$role"
    kubectl_local -n "$namespace" rollout status deployment/checkout-api --timeout=120s
    kubectl_local -n "$namespace" rollout status deployment/worker --timeout=120s
  done
}

clone_branch() {
  local branch="$1"
  local destination="$2"
  rm -rf "$destination"
  git clone --quiet --branch "$branch" "$REPO_DIR" "$destination"
  git -C "$destination" config user.name "driftconverge-injector"
  git -C "$destination" config user.email "driftconverge@example.invalid"
}

commit_and_push() {
  local directory="$1"
  local branch="$2"
  local message="$3"
  git -C "$directory" add manifests
  if git -C "$directory" diff --cached --quiet; then
    return 0
  fi
  git -C "$directory" commit -m "$message" >/dev/null
  git -C "$directory" push --quiet origin "HEAD:$branch"
}

inject_policy_breach() {
  local work_dev="$LAB_DIR/inject-dev"
  local work_prod="$LAB_DIR/inject-prod"
  clone_branch "$DEV_BRANCH" "$work_dev"
  clone_branch "$PROD_BRANCH" "$work_prod"

  sed -i 's|registry.k8s.io/pause:3.9|registry.k8s.io/pause:3.9-debug|g' \
    "$work_dev/manifests/10-checkout-api.yaml"
  sed -i 's|registry.k8s.io/pause:3.9|registry.k8s.io/pause:3.9-debug|g' \
    "$work_prod/manifests/10-checkout-api.yaml"
  sed -i 's|  replicas: 1|  replicas: 3|' "$work_prod/manifests/10-checkout-api.yaml"
  awk '
    { print }
    /              value: prod/ {
      print "            - name: DEBUG_MODE"
      print "              value: \"true\""
    }
  ' "$work_prod/manifests/10-checkout-api.yaml" >"$work_prod/manifests/10-checkout-api.yaml.new"
  mv "$work_prod/manifests/10-checkout-api.yaml.new" "$work_prod/manifests/10-checkout-api.yaml"

  commit_and_push "$work_dev" "$DEV_BRANCH" "inject dev-only debug fields for policy test"
  commit_and_push "$work_prod" "$PROD_BRANCH" "accidentally merge dev-only fields into prod"
}

case "$MODE" in
  reset-git)
    reset_git_baseline
    ;;
  reset-cluster)
    reset_cluster_baseline
    ;;
  reset)
    reset_git_baseline
    reset_cluster_baseline
    ;;
  policy)
    reset_git_baseline
    inject_policy_breach
    ;;
  env-conflict)
    reset_git_baseline
    kubectl_local -n "$PROD_NAMESPACE" set env deployment/checkout-api APP_MODE=unexpected >/dev/null
    kubectl_local -n "$PROD_NAMESPACE" rollout status deployment/checkout-api --timeout=60s
    ;;
  stuck-rollout)
    reset_git_baseline
    kubectl_local -n "$PROD_NAMESPACE" set image deployment/checkout-api \
      app=registry.k8s.io/pause:3.9-stuck >/dev/null
    kubectl_local -n "$PROD_NAMESPACE" rollout status deployment/checkout-api --timeout=5s >/dev/null 2>&1 || true
    ;;
  api-unreachable)
    cat >"$LAB_DIR/prod-unreachable.kubeconfig" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: unreachable
    cluster:
      server: https://127.0.0.1:1
      insecure-skip-tls-verify: true
users:
  - name: unreachable
    user:
      token: invalid-test-token
contexts:
  - name: unreachable
    context:
      cluster: unreachable
      user: unreachable
      namespace: cluster-prod
current-context: unreachable
EOF
    ;;
  *)
    echo "usage: $0 {reset|reset-git|reset-cluster|policy|env-conflict|stuck-rollout|api-unreachable}" >&2
    exit 2
    ;;
esac

printf '{"mode":"%s","injected_at":"%s"}\n' "$MODE" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
