write_base_manifests() {
  local directory="$1"
  local role="$2"
  local namespace replicas dev_env image
  if [[ "$role" == "dev" ]]; then
    namespace="$DEV_NAMESPACE"
    replicas="2"
    dev_env=$'            - name: DEBUG_MODE\n              value: "true"\n'
    image="registry.k8s.io/pause:3.9"
  else
    namespace="$PROD_NAMESPACE"
    replicas="1"
    dev_env=""
    image="registry.k8s.io/pause:3.9"
  fi

  cat >"$directory/manifests/00-namespace.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $namespace
  labels:
    driftconverge.io/seed: "$SEED"
---
EOF

  cat >"$directory/manifests/10-checkout-api.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout-api
  namespace: $namespace
  labels:
    app: checkout-api
    driftconverge.io/role: $role
spec:
  replicas: $replicas
  selector:
    matchLabels:
      app: checkout-api
  template:
    metadata:
      labels:
        app: checkout-api
        driftconverge.io/role: $role
    spec:
      containers:
        - name: app
          image: $image
          imagePullPolicy: IfNotPresent
          env:
            - name: APP_MODE
              value: $role
${dev_env}
---
EOF

  cat >"$directory/manifests/20-worker.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
  namespace: $namespace
  labels:
    app: worker
    driftconverge.io/role: $role
spec:
  replicas: 1
  selector:
    matchLabels:
      app: worker
  template:
    metadata:
      labels:
        app: worker
        driftconverge.io/role: $role
    spec:
      containers:
        - name: app
          image: $image
          imagePullPolicy: IfNotPresent
          env:
            - name: APP_MODE
              value: $role
---
EOF

  if [[ "$role" == "dev" ]]; then
    cat >"$directory/manifests/90-policy.yaml" <<EOF
apiVersion: driftconverge/v1
kind: BranchPolicy
metadata:
  name: dev-to-prod-guard
spec:
  prodReplicaCeiling: 2
  prodForbiddenImageTags:
    - "3.9-debug"
  prodForbiddenEnvKeys:
    - DEBUG_MODE
EOF
  fi
}
