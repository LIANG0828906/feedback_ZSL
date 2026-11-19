from __future__ import annotations

import os
import random

from locust import HttpUser, between, task


TENANTS = [
    item.strip()
    for item in os.environ.get(
        "ORDER_GUARD_TENANTS", "tenant-alpha,tenant-beta,tenant-gamma"
    ).split(",")
    if item.strip()
]
STATUSES = ["created", "paid", "shipped"]


class OrderUser(HttpUser):
    wait_time = between(0.02, 0.08)

    def on_start(self) -> None:
        self.tenant_id = random.choice(TENANTS)
        self.headers = {"X-Tenant-Id": self.tenant_id}

    @task(9)
    def query_orders(self) -> None:
        self.client.get(
            "/api/v1/orders",
            params={
                "status": random.choice(STATUSES),
                "page": 1,
                "page_size": 20,
            },
            headers=self.headers,
            name="/api/v1/orders?status=[status]",
        )

    @task(1)
    def create_order(self) -> None:
        self.client.post(
            "/api/v1/orders",
            json={"amount_cents": random.randint(100, 200000)},
            headers=self.headers,
            name="/api/v1/orders POST",
        )
