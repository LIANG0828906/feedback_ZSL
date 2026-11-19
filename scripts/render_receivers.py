#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tenants", required=True)
    parser.add_argument("--image", default=os.environ.get("WEBHOOK_IMAGE", "webhook-lab:dev"))
    args = parser.parse_args()
    with open(args.tenants, "r", encoding="utf-8") as handle:
        tenants = json.load(handle)["tenants"]

    documents: list[str] = []
    for tenant in tenants:
        tenant_id = tenant["tenant_id"]
        name = f"receiver-{tenant_id}"
        object_labels = (
            "  labels:\n"
            "    app: webhook-receiver\n"
            f"    tenant: {tenant_id}\n"
        )
        pod_labels = (
            "      labels:\n"
            "        app: webhook-receiver\n"
            f"        tenant: {tenant_id}\n"
        )
        documents.append(
            f"""apiVersion: apps/v1
kind: Deployment
metadata:
  name: {name}
{object_labels}spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
  selector:
    matchLabels:
      app: webhook-receiver
      tenant: {tenant_id}
  template:
    metadata:
{pod_labels}    spec:
      terminationGracePeriodSeconds: 5
      containers:
        - name: receiver
          image: {args.image}
          imagePullPolicy: IfNotPresent
          command: ["python", "-m", "receiver.server"]
          env:
            - name: TENANT_ID
              value: "{tenant_id}"
            - name: BASE_LATENCY_MS
              value: "30"
            - name: SLOW_RATIO
              value: "0"
            - name: SLOW_MIN_MS
              value: "500"
            - name: SLOW_MAX_MS
              value: "3000"
            - name: ERROR_RATE
              value: "0"
            - name: PERMANENT_4XX_RATE
              value: "0"
            - name: LOG_LEVEL
              value: "INFO"
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet:
              path: /live
              port: http
            initialDelaySeconds: 1
            periodSeconds: 3
            timeoutSeconds: 1
          livenessProbe:
            httpGet:
              path: /live
              port: http
            initialDelaySeconds: 3
            periodSeconds: 5
            timeoutSeconds: 1
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              cpu: 50m
              memory: 40Mi
---
apiVersion: v1
kind: Service
metadata:
  name: {name}
{object_labels}spec:
  selector:
    app: webhook-receiver
    tenant: {tenant_id}
  ports:
    - name: http
      port: 8080
      targetPort: http
"""
        )
    print("---\n".join(documents))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

