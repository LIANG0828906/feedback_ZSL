"""Command line entry: generate / run."""

import argparse
import json
import os
import sys
import time

from . import core, errors, ingest, manifest as manifest_mod
from .config import load_config, resolve
from .generator import generate
from .scheduler import TenantExecution, execute
from .store import TenantStore


def log(msg):
    print(msg, flush=True)


def cmd_generate(cfg):
    stats = generate(cfg)
    log(f"[generate] batch_records={stats['batch_records']} "
        f"stream_records={stats['stream_records']} "
        f"vehicles={stats['vehicles']} batches={stats['batches']}")
    return 0


def _ingest_tenant(cfg, manifest, tenant):
    data_dir = resolve(cfg, cfg["data_dir"])
    window_ms = cfg["engine"]["window_seconds"] * 1000
    lateness_ms = cfg["engine"]["allowed_lateness_seconds"] * 1000
    fleet = tenant["fleet"]
    batch_recs, batch_in = ingest.read_batch(data_dir, manifest, fleet)
    stream_recs, stream_in, late = ingest.read_stream(
        data_dir, manifest, fleet, window_ms, lateness_ms)
    batch_kept, batch_dropped = core.dedup(batch_recs)
    stream_kept, stream_dropped = core.dedup(stream_recs)
    return {
        "batch": batch_kept,
        "stream": stream_kept,
        "stats": {
            "batch_input": batch_in,
            "batch_dedup_dropped": batch_dropped,
            "stream_input": stream_in,
            "stream_dedup_dropped": stream_dropped,
            "stream_late": late,
        },
    }


def _finalize_tenant(cfg, tenant_cfg, tenant_exec, started):
    """Merge, check batch/stream consistency, build the tenant report."""
    store = tenant_exec.store
    report = {
        "tenant_id": tenant_cfg["tenant_id"],
        "status": "SUCCEEDED" if tenant_exec.failed is None else "FAILED",
        "error_code": tenant_exec.failed,
        "duration_sec": round(time.monotonic() - started, 3),
    }
    if tenant_exec.failed is not None:
        report["completed_vehicles"] = store.completed_vehicles()
        return report, None

    batch_agg = store.merge("batch")
    stream_agg = store.merge("stream")
    mismatches = []
    for key in sorted(set(batch_agg) | set(stream_agg)):
        if batch_agg.get(key) != stream_agg.get(key):
            mismatches.append({"vehicle": key[0], "window_start": key[1]})
    consistency = {"status": "OK" if not mismatches else "MISMATCH",
                   "mismatch_windows": mismatches}
    report["consistency"] = consistency
    if mismatches:
        report["status"] = "FAILED"
        report["error_code"] = errors.E_CONSISTENCY_MISMATCH
        log(f"[{tenant_cfg['tenant_id']}] batch/stream mismatch in "
            f"{len(mismatches)} windows: {errors.E_CONSISTENCY_MISMATCH}")
        return report, None
    results = []
    for (vehicle, ws), (cnt, ss, sf, sd) in sorted(batch_agg.items()):
        results.append({
            "vehicle_id": vehicle,
            "window_start": ws,
            "window_end": ws + cfg["engine"]["window_seconds"] * 1000,
            "record_count": cnt,
            "avg_speed": round(ss / cnt / 10, 3),
            "total_fuel_ml": sf,
            "total_distance_m": sd,
        })
    return report, results


def cmd_run(cfg, resume):
    started = time.monotonic()
    data_dir = resolve(cfg, cfg["data_dir"])
    try:
        manifest = manifest_mod.load_manifest(data_dir)
    except manifest_mod.SchemaViolation as exc:
        log(f"[fatal] {errors.E_SCHEMA_VIOLATION}: {exc}")
        return 2

    engine_cfg = cfg["engine"]
    tenants = []
    ingest_stats = {}
    skew_stats = {}
    unit_loads = {}
    for tenant_cfg in cfg["tenants"]:
        t0 = time.monotonic()
        data = _ingest_tenant(cfg, manifest, tenant_cfg)
        tid = tenant_cfg["tenant_id"]
        ingest_stats[tid] = data["stats"]
        tasks, skew, loads = core.build_tasks(
            tid, {"batch": data["batch"], "stream": data["stream"]},
            engine_cfg["units_per_tenant"], engine_cfg["skew_threshold_rows"])
        skew_stats[tid] = skew
        unit_loads[tid] = loads
        store = TenantStore(
            resolve(cfg, cfg["state_dir"], tid, "state.db"), fresh=not resume)
        tenants.append(TenantExecution(tenant_cfg, tasks, store, engine_cfg))
        log(f"[{tid}] ingested batch={data['stats']['batch_input']} "
            f"stream={data['stats']['stream_input']} "
            f"late={data['stats']['stream_late']} tasks={len(tasks)} "
            f"({time.monotonic() - t0:.1f}s)")

    execute(tenants, engine_cfg, cfg["fault_injection"], log=log)

    out_dir = resolve(cfg, cfg["output_dir"])
    os.makedirs(out_dir, exist_ok=True)
    reports = {}
    exit_code = 0
    for tenant_cfg, tenant_exec in zip(cfg["tenants"], tenants):
        tid = tenant_cfg["tenant_id"]
        stats = ingest_stats[tid]
        report, results = _finalize_tenant(cfg, tenant_cfg, tenant_exec, started)
        total_rows = sum(loads for loads in unit_loads[tid])
        heaviest = max(unit_loads[tid]) if unit_loads[tid] else 0
        report.update({
            "input": {
                "batch_records": stats["batch_input"],
                "stream_records": stats["stream_input"],
            },
            "dedup": {
                "batch_dropped": stats["batch_dedup_dropped"],
                "stream_dropped": stats["stream_dedup_dropped"],
            },
            "stream": {"late_records": stats["stream_late"]},
            "effective_records": {
                "batch": stats["batch_input"] - stats["batch_dedup_dropped"],
                "stream": stats["stream_input"] - stats["stream_dedup_dropped"]
                        - stats["stream_late"],
            },
            "skew": {
                "threshold_rows": engine_cfg["skew_threshold_rows"],
                "split_keys": skew_stats[tid]["split_keys"],
                "split_chunks": skew_stats[tid]["split_chunks"],
            },
            "tasks": {
                "planned": tenant_exec.planned,
                "executed": tenant_exec.executed,
                "resumed_skipped": tenant_exec.pre_committed,
                "retries": tenant_exec.retries,
            },
            "units": [
                {
                    "unit_id": f"u{unit:02d}",
                    "tasks": stat["tasks"],
                    "rows": stat["rows"],
                    "peak_mem_bytes": stat["peak_mem_bytes"],
                }
                for unit, stat in sorted(tenant_exec.unit_stats.items())
            ],
            "heaviest_unit_row_share": (
                round(heaviest / total_rows, 6) if total_rows else 0.0),
            "faults": {
                "worker_crashes": tenant_exec.crashes,
                "task_retries": tenant_exec.retries,
            },
        })
        if "consistency" not in report:
            report["consistency"] = {"status": "SKIPPED", "mismatch_windows": []}
        reports[tid] = report
        if results is not None:
            tenant_out = os.path.join(out_dir, tid)
            os.makedirs(tenant_out, exist_ok=True)
            with open(os.path.join(tenant_out, "results.json"),
                      "w", encoding="utf-8") as fh:
                json.dump({"tenant_id": tid, "results": results}, fh,
                          indent=2, sort_keys=True)
            log(f"[{tid}] wrote {len(results)} vehicle-windows "
                f"-> {tenant_out}")
        else:
            exit_code = 1
            log(f"[{tid}] job failed: {report['error_code']}")
        tenant_exec.store.close()

    report_doc = {
        "engine": {
            "units_per_tenant": engine_cfg["units_per_tenant"],
            "window_seconds": engine_cfg["window_seconds"],
            "allowed_lateness_seconds": engine_cfg["allowed_lateness_seconds"],
            "worker_processes": engine_cfg["worker_processes"],
        },
        "tenants": reports,
    }
    with open(os.path.join(out_dir, "report.json"), "w", encoding="utf-8") as fh:
        json.dump(report_doc, fh, indent=2, sort_keys=True)
    log(f"[run] finished in {time.monotonic() - started:.1f}s "
        f"-> {os.path.join(out_dir, 'report.json')}")
    return exit_code


def main(argv=None):
    parser = argparse.ArgumentParser(prog="telemetry_engine")
    parser.add_argument("command", choices=["generate", "run"])
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--resume", action="store_true",
                        help="resume from committed per-tenant state")
    args = parser.parse_args(argv)
    cfg = load_config(args.config)
    if args.command == "generate":
        return cmd_generate(cfg)
    return cmd_run(cfg, resume=args.resume)


if __name__ == "__main__":
    sys.exit(main())
