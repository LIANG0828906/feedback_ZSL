#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE="${NAMESPACE:-webhook-lab}"
IMAGE="${WEBHOOK_IMAGE:-webhook-lab:dev}"
PYTHON="${PYTHON:-python3}"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl is required" >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "Docker is required" >&2
  exit 1
fi
if ! command -v kind >/dev/null 2>&1; then
  echo "kind is required" >&2
  exit 1
fi

CONTEXT="$(kubectl config current-context)"
if [[ -n "${KIND_CLUSTER_NAME:-}" ]]; then
  CLUSTER_NAME="$KIND_CLUSTER_NAME"
elif [[ "$CONTEXT" == kind-* ]]; then
  CLUSTER_NAME="${CONTEXT#kind-}"
else
  echo "current context $CONTEXT is not a kind context; set KIND_CLUSTER_NAME" >&2
  exit 1
fi

echo "Building $IMAGE..."
docker build -t "$IMAGE" "$ROOT"
echo "Loading $IMAGE into kind cluster $CLUSTER_NAME..."
kind load docker-image "$IMAGE" --name "$CLUSTER_NAME"

if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  kubectl create namespace "$NAMESPACE"
fi
kubectl config set-context --current --namespace="$NAMESPACE" >/dev/null

kubectl apply -f "$ROOT/deploy/quota.yaml"
kubectl create configmap tenant-config \
  --from-file=tenants.json="$ROOT/deploy/tenants.json" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl scale deployment/webhook-dispatcher --replicas=0 >/dev/null 2>&1 || true
kubectl wait --for=delete pod -l app=webhook-dispatcher --timeout=60s \
  >/dev/null 2>&1 || true

kubectl delete job webhook-db-init --ignore-not-found >/dev/null
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: webhook-db-init
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: webhook-db-init
    spec:
      restartPolicy: Never
      containers:
        - name: init
          image: $IMAGE
          imagePullPolicy: IfNotPresent
          command:
            - sh
            - -c
            - rm -f /data/webhook-lab.db /data/webhook-lab.db-wal /data/webhook-lab.db-shm && chmod 0777 /data
          resources:
            requests:
              cpu: 5m
              memory: 8Mi
            limits:
              cpu: 50m
              memory: 32Mi
          volumeMounts:
            - name: coordinator-data
              mountPath: /data
      volumes:
        - name: coordinator-data
          hostPath:
            path: /var/lib/webhook-lab
            type: DirectoryOrCreate
EOF
kubectl wait --for=condition=complete job/webhook-db-init --timeout=120s
kubectl delete job webhook-db-init --ignore-not-found >/dev/null

"$PYTHON" "$ROOT/scripts/render_receivers.py" \
  --tenants "$ROOT/deploy/tenants.json" \
  --image "$IMAGE" | kubectl apply -f -
kubectl apply -f "$ROOT/deploy/dispatcher.yaml"

echo "Waiting for dispatcher rollout..."
kubectl rollout status deployment/webhook-dispatcher --timeout=180s

echo "Waiting for receiver rollouts..."
while IFS= read -r deployment; do
  kubectl rollout status "$deployment" --timeout=180s >/dev/null
done < <(kubectl get deployment -l app=webhook-receiver -o name)

echo "Namespace $NAMESPACE is ready."
kubectl get pods -l 'app in (webhook-dispatcher,webhook-receiver)' -o wide

