#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/env.sh"

REPO_ONLY=false
if [[ "${1:-}" == "--repo-only" || "${2:-}" == "--repo-only" ]]; then
  REPO_ONLY=true
fi

if [[ "${1:-}" == "--force" || "${2:-}" == "--force" ]]; then
  rm -rf "$LAB_DIR/repo.git" "$LAB_DIR/work" "$LAB_DIR/apply"
fi

mkdir -p "$LAB_DIR"

if [[ -d "$REPO_DIR" ]]; then
  echo "bare repository already exists: $REPO_DIR"
else
  git init --bare --initial-branch="$DEV_BRANCH" "$REPO_DIR" >/dev/null
fi

if ! git --git-dir="$REPO_DIR" show-ref --verify --quiet "refs/heads/$DEV_BRANCH"; then
  rm -rf "$WORK_DIR"
  git init --initial-branch="$DEV_BRANCH" "$WORK_DIR" >/dev/null
  git -C "$WORK_DIR" config user.name "driftconverge-lab"
  git -C "$WORK_DIR" config user.email "driftconverge@example.invalid"
  mkdir -p "$WORK_DIR/manifests"

  source "$SCRIPT_DIR/write_base_manifests.sh"
  write_base_manifests "$WORK_DIR" "dev"
  git -C "$WORK_DIR" add manifests
  GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" \
    git -C "$WORK_DIR" commit -m "seed $SEED: initial cluster-dev snapshot" >/dev/null
  git -C "$WORK_DIR" tag -f "drift-baseline-$DEV_BRANCH" HEAD
  git -C "$WORK_DIR" remote add origin "$REPO_DIR"
  git -C "$WORK_DIR" push --quiet origin "$DEV_BRANCH" "refs/tags/drift-baseline-$DEV_BRANCH"

  git -C "$WORK_DIR" checkout -b "$PROD_BRANCH" >/dev/null
  rm -f "$WORK_DIR/manifests/90-policy.yaml"
  write_base_manifests "$WORK_DIR" "prod"
  git -C "$WORK_DIR" add -A manifests
  GIT_AUTHOR_DATE="2026-01-01T00:00:01Z" GIT_COMMITTER_DATE="2026-01-01T00:00:01Z" \
    git -C "$WORK_DIR" commit -m "seed $SEED: initial cluster-prod snapshot" >/dev/null
  git -C "$WORK_DIR" tag -f "drift-baseline-$PROD_BRANCH" HEAD
  git -C "$WORK_DIR" push --quiet origin "$PROD_BRANCH" "refs/tags/drift-baseline-$PROD_BRANCH"
  git --git-dir="$REPO_DIR" symbolic-ref HEAD "refs/heads/$DEV_BRANCH"
fi

if [[ "$REPO_ONLY" == "true" ]]; then
  cat <<EOF
{"seed": $SEED, "repository": "$REPO_DIR", "mode": "repo-only"}
EOF
  exit 0
fi

if ! kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  kind create cluster \
    --name "$KIND_CLUSTER" \
    --config "$SCRIPT_DIR/kind-config.yaml" \
    --wait 120s
fi
kind export kubeconfig --name "$KIND_CLUSTER" --kubeconfig "$LAB_DIR/kind.kubeconfig" >/dev/null

extract_branch "$DEV_BRANCH" "$LAB_DIR/apply-dev"
extract_branch "$PROD_BRANCH" "$LAB_DIR/apply-prod"
apply_snapshot_manifests "$LAB_DIR/apply-dev"
apply_snapshot_manifests "$LAB_DIR/apply-prod"
kubectl_local -n "$DEV_NAMESPACE" rollout status deployment/checkout-api --timeout=120s
kubectl_local -n "$DEV_NAMESPACE" rollout status deployment/worker --timeout=120s
kubectl_local -n "$PROD_NAMESPACE" rollout status deployment/checkout-api --timeout=120s
kubectl_local -n "$PROD_NAMESPACE" rollout status deployment/worker --timeout=120s

"$SCRIPT_DIR/init_db.sh"

cat <<EOF
{
  "seed": $SEED,
  "repository": "$REPO_DIR",
  "kubeconfig": "$LAB_DIR/kind.kubeconfig",
  "kind_cluster": "$KIND_CLUSTER",
  "dev_branch": "$DEV_BRANCH",
  "prod_branch": "$PROD_BRANCH",
  "dev_namespace": "$DEV_NAMESPACE",
  "prod_namespace": "$PROD_NAMESPACE",
  "database": "$PGDATABASE_LOCAL"
}
EOF
