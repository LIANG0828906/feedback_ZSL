#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE="${NAMESPACE:-webhook-lab}"
IMAGE="${WEBHOOK_IMAGE:-webhook-lab:dev}"

if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  kubectl config set-context --current --namespace=default >/dev/null 2>&1 || true
  echo "namespace $NAMESPACE does not exist"
  exit 0
fi

kubectl -n "$NAMESPACE" scale deployment/webhook-dispatcher --replicas=0 >/dev/null 2>&1 || true
kubectl -n "$NAMESPACE" delete job webhook-data-cleanup --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: webhook-data-cleanup
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: cleanup
          image: $IMAGE
          imagePullPolicy: IfNotPresent
          command: ["sh", "-c", "rm -rf /data/*"]
          volumeMounts:
            - name: coordinator-data
              mountPath: /data
      volumes:
        - name: coordinator-data
          hostPath:
            path: /var/lib/webhook-lab
            type: DirectoryOrCreate
EOF
kubectl -n "$NAMESPACE" wait --for=condition=complete job/webhook-data-cleanup --timeout=120s
kubectl delete namespace "$NAMESPACE" --wait=true
kubectl config set-context --current --namespace=default >/dev/null
echo "Deleted namespace $NAMESPACE and its coordinator data."

