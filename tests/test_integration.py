from __future__ import annotations

import json
import threading
import unittest
import urllib.error
import urllib.request
from collections import Counter

from poweraudit.database import Database
from poweraudit.service import (
    ApiError,
    PowerAuditService,
    create_http_server,
)

try:
    from .helpers import DemoEnvironment
except ImportError:  # Allows `python -m unittest discover -s tests`.
    from helpers import DemoEnvironment  # type: ignore


def _semantic_audit(audit: dict) -> dict:
    return {
        "totals": audit["totals"],
        "servers": audit["servers"],
        "devices": audit["devices"],
        "risk_ranking": audit["risk_ranking"],
        "unknown_devices": audit["unknown_devices"],
        "declaration_mismatches": audit["declaration_mismatches"],
    }


class NormalTopologyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.env = DemoEnvironment().start()
        cls.service = PowerAuditService(
            cls.env.config,
            cls.env.inventory,
            Database(cls.env.config.database_path),
        )
        cls.audit = cls.service.run_audit("IT-NORMAL-001")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.env.close()

    def test_audit_counts_and_overlap_levels(self) -> None:
        self.assertEqual(self.audit["totals"]["servers"], 96)
        self.assertEqual(self.audit["totals"]["devices"], 21)
        self.assertEqual(self.audit["totals"]["independent_servers"], 66)
        self.assertEqual(self.audit["totals"]["pseudo_redundant_servers"], 24)
        self.assertEqual(self.audit["totals"]["single_path_servers"], 6)
        self.assertEqual(self.audit["totals"]["topology_unknown_servers"], 0)
        overlaps = Counter(
            tuple(server["overlap_levels"])
            for server in self.audit["servers"]
            if server["redundancy_status"] == "pseudo_redundant"
        )
        self.assertEqual(overlaps[("pdu", "ups", "upstream_branch")], 14)
        self.assertEqual(overlaps[("ups",)], 6)
        self.assertEqual(overlaps[("upstream_branch",)], 4)
        self.assertEqual(self.audit["totals"]["declaration_mismatch_servers"], 14)

    def test_risk_ranking_and_exposure_lists(self) -> None:
        self.assertEqual(self.audit["risk_ranking"][0]["device_id"], "UPS-01")
        pdu = next(
            item
            for item in self.audit["devices"]
            if item["device_id"] == "PDU-AUX-17"
        )
        self.assertEqual(pdu["all_outage_count"], 14)
        self.assertEqual(len(pdu["all_outage_servers"]), 14)
        self.assertEqual(pdu["all_outage_servers"], sorted(pdu["all_outage_servers"]))
        for previous, current in zip(
            self.audit["risk_ranking"], self.audit["risk_ranking"][1:]
        ):
            self.assertGreaterEqual(
                previous["all_outage_count"], current["all_outage_count"]
            )

    def test_duplicate_batch_is_rejected_and_immutable(self) -> None:
        before = self.service.database.get_audit("IT-NORMAL-001")
        with self.assertRaises(ApiError) as context:
            self.service.run_audit("IT-NORMAL-001")
        self.assertEqual(context.exception.status, 409)
        self.assertEqual(context.exception.code, "AUDIT_BATCH_EXISTS")
        after = self.service.database.get_audit("IT-NORMAL-001")
        self.assertEqual(before, after)

    def test_repeated_audit_is_deterministic(self) -> None:
        second = self.service.run_audit("IT-NORMAL-002")
        self.assertEqual(
            _semantic_audit(self.audit),
            _semantic_audit(second),
        )
        self.assertLessEqual(
            second["duration_ms"],
            self.env.config.total_timeout_seconds * 1000.0,
        )

    def test_maintenance_plan_is_blocked_and_ordered(self) -> None:
        plan = self.service.create_maintenance_plan(
            {
                "batch_id": "IT-NORMAL-001",
                "target_type": "pdu",
                "target_id": "PDU-AUX-17",
            }
        )
        self.assertFalse(plan["executable"])
        self.assertEqual(plan["counts"]["full_outage"], 14)
        self.assertEqual(plan["counts"]["one_path_loss"], 0)
        self.assertEqual(
            [item["impact"] for item in plan["servers"]],
            ["full_outage"] * 14,
        )
        self.assertTrue(plan["blocked_servers"])
        self.assertEqual(
            plan["blocking_reasons"][0]["code"], "UNAPPROVED_FULL_OUTAGE"
        )
        self.assertEqual(
            len({item["server_id"] for item in plan["servers"]}), 14
        )

    def test_acceptable_servers_can_make_plan_executable(self) -> None:
        blocked = self.service.create_maintenance_plan(
            {
                "batch_id": "IT-NORMAL-001",
                "target_type": "pdu",
                "target_id": "PDU-AUX-17",
            }
        )
        plan = self.service.create_maintenance_plan(
            {
                "batch_id": "IT-NORMAL-001",
                "target_type": "pdu",
                "target_id": "PDU-AUX-17",
                "acceptable_server_ids": blocked["blocked_servers"],
            }
        )
        self.assertTrue(plan["executable"])
        self.assertEqual(plan["blocked_servers"], [])

    def test_emergency_order_and_server_position(self) -> None:
        plan = self.service.create_emergency_plan(
            {"batch_id": "IT-NORMAL-001"}
        )
        self.assertEqual(plan["trigger"]["ups_ids"], ["UPS-01"])
        self.assertGreater(plan["counts"]["priority_1"], 0)
        self.assertGreater(plan["counts"]["priority_2"], 0)
        positions = [item["position"] for item in plan["sequence"]]
        self.assertEqual(positions, list(range(1, len(positions) + 1)))
        priorities = [item["priority"] for item in plan["sequence"]]
        self.assertEqual(priorities, sorted(priorities))
        for tier in (1, 2):
            tier_items = [item for item in plan["sequence"] if item["priority"] == tier]
            self.assertEqual(
                [(item["business_cost"], item["server_id"]) for item in tier_items],
                sorted(
                    (item["business_cost"], item["server_id"])
                    for item in tier_items
                ),
            )
        target = plan["sequence"][0]
        persisted = self.service.database.get_plan(plan["plan_id"])
        from poweraudit.planner import Planner

        answer = Planner.server_position(persisted, target["server_id"])
        self.assertIsNotNone(answer)
        self.assertEqual(answer["position"], target["position"])
        self.assertIn("basis", answer)
        self.assertIn("reason", answer)

    def test_status_and_metrics(self) -> None:
        status = self.service.status("IT-NORMAL-001")
        self.assertEqual(status["batch_id"], "IT-NORMAL-001")
        self.assertEqual(status["topology_unknown_devices"], [])
        self.assertTrue(status["highest_risk_devices"])
        serialized = json.dumps(status, sort_keys=True)
        self.assertNotIn(str(self.env.base), serialized)
        metrics = self.service.metrics()
        for name in (
            "poweraudit_device_online",
            "poweraudit_device_server_count",
            "poweraudit_device_all_outage_servers",
            "poweraudit_device_pseudo_redundant_servers",
            "poweraudit_device_single_path_servers",
            "poweraudit_current_audit_batch_info",
            "poweraudit_infeasible_plans",
        ):
            self.assertIn(name, metrics)
        self.assertIn('device_id="UPS-01"', metrics)
        self.assertIn('device_id="PDU-AUX-17"', metrics)

    def test_history_survives_database_reopen(self) -> None:
        reopened = Database(self.env.config.database_path)
        self.assertEqual(
            reopened.get_audit("IT-NORMAL-001")["totals"],
            self.audit["totals"],
        )
        self.assertTrue(reopened.list_audits(10))

    def test_http_api(self) -> None:
        server = create_http_server(self.service, port=0)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        host, port = server.server_address[:2]
        base = f"http://{host}:{port}"
        try:
            with urllib.request.urlopen(
                base + "/api/v1/status?batch_id=IT-NORMAL-001", timeout=3
            ) as response:
                self.assertEqual(response.status, 200)
                payload = json.loads(response.read())
                self.assertEqual(payload["batch_id"], "IT-NORMAL-001")
            request = urllib.request.Request(
                base + "/api/v1/audits",
                data=b'{"batch_id":"IT-NORMAL-001"}',
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as context:
                urllib.request.urlopen(request, timeout=3)
            http_error = context.exception
            self.assertEqual(http_error.code, 409)
            error = json.loads(http_error.read())
            http_error.close()
            self.assertEqual(error["error"]["code"], "AUDIT_BATCH_EXISTS")
            with urllib.request.urlopen(base + "/metrics", timeout=3) as response:
                self.assertIn(b"poweraudit_device_online", response.read())
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


class UnknownTopologyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.env = DemoEnvironment(disabled={"PDU-R01-A"}).start()
        cls.service = PowerAuditService(
            cls.env.config,
            cls.env.inventory,
            Database(cls.env.config.database_path),
        )
        cls.audit = cls.service.run_audit("IT-UNKNOWN-001")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.env.close()

    def test_unknown_device_is_separate_and_rest_is_audited(self) -> None:
        self.assertEqual(self.audit["totals"]["topology_unknown_devices"], 1)
        unknown = self.audit["unknown_devices"][0]
        self.assertEqual(unknown["device_id"], "PDU-R01-A")
        self.assertIn("TIMEOUT", unknown["reasons"])
        ranked_ids = {item["device_id"] for item in self.audit["risk_ranking"]}
        self.assertNotIn("PDU-R01-A", ranked_ids)
        self.assertEqual(self.audit["totals"]["online_devices"], 20)
        self.assertGreater(self.audit["totals"]["independent_servers"], 0)
        self.assertLessEqual(
            self.audit["duration_ms"],
            self.env.config.total_timeout_seconds * 1000.0,
        )

    def test_downstream_paths_are_unknown(self) -> None:
        affected = next(
            server
            for server in self.audit["servers"]
            if any(
                path["pdu_id"] == "PDU-R01-A" for path in server["power_paths"]
            )
        )
        self.assertEqual(affected["redundancy_status"], "topology_unknown")
        down_path = next(
            path
            for path in affected["power_paths"]
            if path["pdu_id"] == "PDU-R01-A"
        )
        self.assertFalse(down_path["topology_known"])
        self.assertIn("PDU_UNREACHABLE", down_path["topology_reasons"])


class GlobalTimeoutTests(unittest.TestCase):
    def test_all_devices_unreachable_still_respects_total_deadline(self) -> None:
        env = DemoEnvironment(
            disabled={
                "UPS-01",
                "UPS-02",
                "UPS-03",
                "UPS-04",
            },
            total_timeout_seconds=0.3,
            device_timeout_seconds=0.2,
            request_timeout_seconds=0.05,
        )
        try:
            # Disable every PDU as well.
            from poweraudit.simulator import build_agent_server, build_simulated_devices

            env.simulator = build_agent_server(
                build_simulated_devices(
                    env.inventory, env.scenario, enabled_device_ids=set()
                ),
                backend="auto",
            )
            env.start()
            service = PowerAuditService(
                env.config,
                env.inventory,
                Database(env.config.database_path),
            )
            audit = service.run_audit("IT-TIMEOUT-001")
            self.assertEqual(audit["totals"]["online_devices"], 0)
            self.assertEqual(audit["totals"]["topology_unknown_devices"], 21)
            self.assertLessEqual(audit["duration_ms"], 300.0)
        finally:
            env.close()


if __name__ == "__main__":
    unittest.main()
