from __future__ import annotations

import json
import tempfile
import time
import unittest
from dataclasses import replace
from pathlib import Path

from settlement_rollout.config import load_configuration
from settlement_rollout.injections import load_injections
from settlement_rollout.loadgen import LoadGenerator, LoadTarget
from settlement_rollout.orchestrator import RolloutOrchestrator
from settlement_rollout.scenario import (
    _allocate_ports,
    _shutdown_instances,
    _start_supervisor,
    _stop_processes,
    _wait_instance,
)
from settlement_rollout.store import AuditStore

from .helpers import (
    QUICK_PLAN,
    QUICK_SLO,
    SUCCESS_INJECTION,
    write_json,
)


class ResumeTests(unittest.TestCase):
    def test_observation_resumes_from_committed_state(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            plan_value = json.loads(QUICK_PLAN.read_text(encoding="utf-8"))
            plan_value["release_id"] = "resume-test"
            plan_value["observation_window_seconds"] = 2.5
            plan_value["sampling"] = {
                "interval_seconds": 0.25,
                "quota_per_instance": 4,
            }
            plan_value["batches"] = [
                {
                    "id": "all-instances",
                    "order": 1,
                    "instances": [
                        instance["id"] for instance in plan_value["instances"]
                    ],
                    "rollback_version": "v1",
                }
            ]
            plan_path = write_json(directory / "plan.json", plan_value)
            slo_value = json.loads(QUICK_SLO.read_text(encoding="utf-8"))
            slo_value["minimum_samples"] = 8
            slo_path = write_json(directory / "slo.json", slo_value)
            config = load_configuration(plan_path, slo_path)
            injection = load_injections(SUCCESS_INJECTION, config)
            host = config.network.host
            ports = _allocate_ports(
                host,
                config.network.port_range[0],
                config.network.port_range[1],
                len(config.instances),
                preferred=[item.port for item in config.instances],
            )
            instances = tuple(
                replace(instance, port=port)
                for instance, port in zip(config.instances, ports)
            )
            config = replace(
                config,
                instances=instances,
                storage=replace(
                    config.storage,
                    database_path=directory / "state.sqlite3",
                    report_path=directory / "report.json",
                ),
            )

            processes = []
            store = None
            load = None
            try:
                for instance in config.instances:
                    processes.append(
                        _start_supervisor(
                            config,
                            instance.instance_id,
                            injection,
                        )
                    )
                for instance in config.instances:
                    _wait_instance(host, instance.port)
                endpoints = {
                    instance.instance_id: f"http://{host}:{instance.port}"
                    for instance in config.instances
                }
                store = AuditStore(config.storage.database_path)
                first = RolloutOrchestrator(config, store, endpoints)
                first.run_id = store.start_run(config)
                batch_id = config.batches[0].batch_id
                store.transition_batch(
                    first.run_id,
                    batch_id,
                    "PRECHECKING",
                    "TEST_START",
                    {},
                )
                first._switch_batch(batch_id)
                first._start_observation(batch_id)
                load = LoadGenerator(
                    [
                        LoadTarget(
                            instance.instance_id,
                            instance.group,
                            host,
                            instance.port,
                        )
                        for instance in config.instances
                    ],
                    rate_per_second=24,
                    workers=16,
                )
                load.start()
                time.sleep(1.0)
                batch = store.get_batch(first.run_id, batch_id)
                cursors = store.max_sample_ids(first.run_id, batch_id)
                first._drain_batch_samples(
                    batch_id,
                    config.batches[0].instances,
                    cursors,
                )
                self.assertGreater(
                    len(store.load_samples(first.run_id, batch_id)),
                    0,
                )

                second = RolloutOrchestrator(config, store, endpoints)
                summary = second.run()
                self.assertEqual(summary["status"], "RELEASED")
                final_batch = store.get_batch(second.run_id, batch_id)
                self.assertEqual(final_batch["status"], "PASSED")
                self.assertGreaterEqual(
                    store.latest_decision(
                        second.run_id, batch_id
                    )["sample_count"],
                    8,
                )
            finally:
                if load is not None:
                    load.stop()
                _shutdown_instances(list(config.instances), host)
                time.sleep(0.1)
                _stop_processes(processes)
                if store is not None:
                    store.close()


if __name__ == "__main__":
    unittest.main()
