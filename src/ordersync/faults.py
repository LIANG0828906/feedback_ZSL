from __future__ import annotations

import os
import subprocess
import threading
from typing import Any

from .config import EndpointConfig
from .db import Connector, pyodbc_connector
from .errors import AppError
from .replication import ReplicaMonitor
from .service import OrderService


class ReplicationFaultController:
    def __init__(
        self,
        service: OrderService,
        monitor: ReplicaMonitor,
        connector: Connector = pyodbc_connector,
    ) -> None:
        self._service = service
        self._monitor = monitor
        self._connector = connector
        self._lock = threading.Lock()

    def disconnect_replication(self) -> dict[str, Any]:
        return self._set_replication(disconnected=True)

    def reconnect_replication(self) -> dict[str, Any]:
        return self._set_replication(disconnected=False)

    def _set_replication(self, *, disconnected: bool) -> dict[str, Any]:
        with self._lock:
            jobs = self._service.config.faults.replication_jobs
            disconnect_command = (
                self._service.config.faults.disconnect_command
            )
            reconnect_command = (
                self._service.config.faults.reconnect_command
            )
            if disconnected and disconnect_command:
                self._run_external_command(disconnect_command)
                mode = "external_command"
            elif not disconnected and reconnect_command:
                self._run_external_command(reconnect_command)
                mode = "external_command"
            elif not jobs:
                if not self._service.config.faults.allow_simulated_replication_stall:
                    raise AppError(
                        "E_FAULT_CONTROL",
                        "No replication fault-control command or jobs are configured",
                        503,
                        {"reason": "fault_control_missing"},
                    )
                mode = "simulated"
            else:
                for job in jobs:
                    endpoint = (
                        self._service.config.primary
                        if job.endpoint == "primary"
                        else self._service.config.replica
                    )
                    try:
                        self._set_job_enabled(
                            endpoint, job.job_name, not disconnected
                        )
                        self._run_job_action(
                            endpoint,
                            "sp_stop_job" if disconnected else "sp_start_job",
                            job.job_name,
                            ignore_text=(
                                "not running"
                                if disconnected
                                else "already running"
                            ),
                        )
                    except Exception:
                        raise AppError(
                            "E_FAULT_CONTROL",
                            "Replication job control failed",
                            503,
                            {
                                "reason": "job_control_failed",
                                "action": (
                                    "disconnect" if disconnected else "reconnect"
                                ),
                            },
                        ) from None
                mode = "sql_agent_job"

            self._monitor.set_forced_disconnected(disconnected)
            try:
                self._monitor.collect_once()
            except Exception:
                pass
            return {
                "replication": (
                    "disconnected" if disconnected else "reconnected"
                ),
                "mode": mode,
                "jobs": [
                    {"endpoint": job.endpoint, "job_name": job.job_name}
                    for job in jobs
                ],
            }

    def _run_external_command(self, command: tuple[str, ...]) -> None:
        environment = os.environ.copy()
        environment.update(
            {
                "ORDERSYNC_PRIMARY_CONNECTION_STRING": (
                    self._service.config.primary.connection_string
                ),
                "ORDERSYNC_PRIMARY_DATABASE": (
                    self._service.config.primary.database
                ),
                "ORDERSYNC_REPLICA_CONNECTION_STRING": (
                    self._service.config.replica.connection_string
                ),
                "ORDERSYNC_REPLICA_DATABASE": (
                    self._service.config.replica.database
                ),
                "ORDERSYNC_REPLICATION_MODE": (
                    self._service.config.replication.mode
                ),
            }
        )
        try:
            completed = subprocess.run(
                list(command),
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=60,
                check=False,
            )
        except Exception:
            raise AppError(
                "E_FAULT_CONTROL",
                "Replication fault-control command failed",
                503,
                {"reason": "command_execution_failed"},
            ) from None
        if completed.returncode != 0:
            raise AppError(
                "E_FAULT_CONTROL",
                "Replication fault-control command returned a failure code",
                503,
                {"reason": "command_failed"},
            )

    def _set_job_enabled(
        self, endpoint: EndpointConfig, job_name: str, enabled: bool
    ) -> None:
        connection = self._connector(endpoint)
        cursor = None
        try:
            cursor = connection.cursor()
            cursor.execute(
                "EXEC msdb.dbo.sp_update_job @job_name = ?, @enabled = ?",
                (job_name, 1 if enabled else 0),
            )
            connection.commit()
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

    def _run_job_action(
        self,
        endpoint: EndpointConfig,
        procedure: str,
        job_name: str,
        *,
        ignore_text: str,
    ) -> None:
        connection = self._connector(endpoint)
        cursor = None
        try:
            cursor = connection.cursor()
            try:
                cursor.execute(
                    f"EXEC msdb.dbo.{procedure} @job_name = ?", (job_name,)
                )
                connection.commit()
            except Exception as exc:
                if ignore_text in str(exc).casefold():
                    return
                raise
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

