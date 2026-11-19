#!/usr/bin/env bash

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB_DIR="${DRIFT_LAB_DIR:-$ROOT_DIR/lab}"
REPO_DIR="${DRIFT_GIT_REPO:-$LAB_DIR/repo.git}"
WORK_DIR="$LAB_DIR/work"
ARTIFACT_DIR="${DRIFT_ARTIFACT_DIR:-$ROOT_DIR/artifacts/scenario}"
KIND_CLUSTER="${DRIFT_KIND_CLUSTER:-drift-lab}"
DEV_BRANCH="${DRIFT_BRANCH_DEV:-cluster-dev}"
PROD_BRANCH="${DRIFT_BRANCH_PROD:-cluster-prod}"
DEV_NAMESPACE="${DRIFT_NAMESPACE_DEV:-cluster-dev}"
PROD_NAMESPACE="${DRIFT_NAMESPACE_PROD:-cluster-prod}"
SEED="${DRIFT_SEED:-315}"
if [[ ! "$SEED" =~ ^[0-9]+$ ]]; then
  echo "DRIFT_SEED must be a non-negative integer" >&2
  exit 2
fi
PGHOST_LOCAL="${DRIFT_PGHOST:-127.0.0.1}"
PGPORT_LOCAL="${DRIFT_PGPORT:-5432}"
PGUSER_LOCAL="${DRIFT_PGUSER:-drift}"
PGDATABASE_LOCAL="${DRIFT_PGDATABASE:-drift315}"
DRIFT_DATABASE_URL="${DRIFT_DATABASE_URL:-postgres://${PGUSER_LOCAL}@${PGHOST_LOCAL}:${PGPORT_LOCAL}/${PGDATABASE_LOCAL}?sslmode=disable}"
export DRIFT_DATABASE_URL
export DRIFT_GIT_REPO="$REPO_DIR"
export DRIFT_MANIFEST_PATH="${DRIFT_MANIFEST_PATH:-manifests}"
export DRIFT_BRANCH_DEV="$DEV_BRANCH"
export DRIFT_BRANCH_PROD="$PROD_BRANCH"
export DRIFT_NAMESPACE_DEV="$DEV_NAMESPACE"
export DRIFT_NAMESPACE_PROD="$PROD_NAMESPACE"

mkdir -p "$LAB_DIR" "$ARTIFACT_DIR"

find_psql() {
  if command -v psql >/dev/null 2>&1; then
    PSQL_BIN="$(command -v psql)"
    return 0
  fi
  local candidate
  candidate="$(find "$HOME" -path '*/usr/lib/postgresql/16/bin/psql' -type f 2>/dev/null | head -1)"
  if [[ -n "$candidate" ]]; then
    local prefix libdir
    prefix="${candidate%/usr/lib/postgresql/16/bin/psql}"
    libdir="$prefix/usr/lib/x86_64-linux-gnu"
    if [[ -d "$libdir" ]]; then
      export LD_LIBRARY_PATH="$libdir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    fi
    PSQL_BIN="$candidate"
    return 0
  fi
  return 1
}

psql_local() {
  find_psql || {
    echo "psql was not found" >&2
    return 1
  }
  "$PSQL_BIN" -h "$PGHOST_LOCAL" -p "$PGPORT_LOCAL" -U "$PGUSER_LOCAL" "$@"
}

kubectl_local() {
  if [[ -f "$LAB_DIR/kind.kubeconfig" ]]; then
    kubectl --kubeconfig "$LAB_DIR/kind.kubeconfig" "$@"
  else
    kubectl --context "kind-${KIND_CLUSTER}" "$@"
  fi
}

extract_branch() {
  local branch="$1"
  local destination="$2"
  rm -rf "$destination"
  mkdir -p "$destination"
  git --git-dir="$REPO_DIR" archive "$branch" | tar -x -C "$destination"
}

apply_snapshot_manifests() {
  local directory="$1"
  local file
  while IFS= read -r file; do
    if grep -Eq '^kind:[[:space:]]+BranchPolicy' "$file"; then
      continue
    fi
    kubectl_local apply -f "$file"
  done < <(find "$directory/manifests" -maxdepth 1 -type f \( -name '*.yaml' -o -name '*.yml' \) | sort)
}

