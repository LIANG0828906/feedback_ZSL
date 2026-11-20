"""无界面运行入口：样本生成、增强、恢复、验证。"""

from __future__ import annotations

import argparse
import json
import sys

import numpy as np

from . import wavio
from .config import EnhanceConfig
from .engine import Engine
from .errors import EnhancerError
from .noise import detect_onset, estimate_noise
from .samplegen import generate_sample


def _cmd_gen_sample(args) -> int:
    info = generate_sample(seed=args.seed, out_dir=args.out, channels=args.channels)
    print(json.dumps(info, ensure_ascii=False, indent=1))
    return 0


def _cmd_run(args) -> int:
    engine = Engine(args.workdir)
    config = EnhanceConfig()
    report = engine.run(
        args.input,
        config,
        trace_name=args.trace,
        resume=args.resume,
        crash_after_channel=args.crash_after_channel,
    )
    paths = engine.export(args.out)
    summary = {
        "output_hash": report["output_hash"],
        "degraded_bands": report["degraded_bands"],
        "export": paths,
    }
    print(json.dumps(summary, ensure_ascii=False, indent=1))
    return 0


def _cmd_verify(args) -> int:
    """验收校验：直达声位置不变、噪声能量下降。"""
    data_in, _ = wavio.read_wav(args.input)
    data_out, _ = wavio.read_wav(args.enhanced)
    config = EnhanceConfig()
    estimates, interval, _ = estimate_noise(
        data_in, config.noise, config.metric.signal_window
    )
    p_start, p_end = interval
    ok = True
    for ch in range(data_in.shape[1]):
        est = estimates[ch]
        onset_in = detect_onset(data_in[:, ch], est.rms)
        onset_out = detect_onset(data_out[:, ch], est.rms)
        e_in = float(np.sum(data_in[p_start:p_end, ch] ** 2))
        e_out = float(np.sum(data_out[p_start:p_end, ch] ** 2))
        pos_ok = onset_in == onset_out
        noise_ok = e_out < e_in
        ok = ok and pos_ok and noise_ok
        print(
            f"ch{ch}: onset {onset_in}=={onset_out} [{'OK' if pos_ok else 'FAIL'}] "
            f"noise_energy {e_in:.6e}->{e_out:.6e} "
            f"({10*np.log10(max(e_out,1e-30)/e_in):+.2f} dB) "
            f"[{'OK' if noise_ok else 'FAIL'}]"
        )
    print("VERIFY:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def _cmd_gui(args) -> int:
    from .gui import run_gui

    return run_gui()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="ir_enhancer", description="脉冲响应噪声抑制与增强处理器"
    )
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("gen-sample", help="生成确定性含噪脉冲响应样本")
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--channels", type=int, default=6)
    p.add_argument("--out", default="work/samples")
    p.set_defaults(func=_cmd_gen_sample)

    p = sub.add_parser("run", help="执行增强并原子导出")
    p.add_argument("--in", dest="input", required=True)
    p.add_argument("--workdir", default="work/run")
    p.add_argument("--out", default="work/export")
    p.add_argument("--trace", default="trace.jsonl")
    p.add_argument("--resume", action="store_true")
    p.add_argument("--crash-after-channel", type=int, default=None)
    p.set_defaults(func=_cmd_run)

    p = sub.add_parser("verify", help="校验直达声位置不变与噪声能量下降")
    p.add_argument("--in", dest="input", required=True)
    p.add_argument("--enhanced", required=True)
    p.set_defaults(func=_cmd_verify)

    p = sub.add_parser("gui", help="启动图形界面（需 PySide6）")
    p.set_defaults(func=_cmd_gui)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except EnhancerError as exc:
        print(json.dumps({"error": exc.to_dict()}, ensure_ascii=False), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
