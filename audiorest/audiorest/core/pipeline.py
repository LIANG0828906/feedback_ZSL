"""确定性分帧修复链：STFT -> 噪声谱估计 -> 谱减 -> 掩蔽平滑 -> 噪声门。

实现不依赖任何随机源，同参数同输入必然逐样本一致，是三条处理路径
（实时/断点恢复/全量重算）输出一致性的基础。算法版本见 ``ALGO_VERSION``。
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from ..errors import raise_para
from ..model import RestoreProject, StageKind


def validate_params(project: RestoreProject) -> None:
    """校验分帧与阶段参数，越界抛 E_PARA 并指出字段名。"""
    framing = project.framing
    if not isinstance(framing.frame_size, int) or not (8 <= framing.frame_size <= 1 << 18):
        raise_para("frame_size 须为 [8, 262144] 内整数", "framing.frame_size",
                   value=framing.frame_size)
    if not isinstance(framing.hop_size, int) or not (1 <= framing.hop_size <= framing.frame_size):
        raise_para("hop_size 须为 [1, frame_size] 内整数", "framing.hop_size",
                   value=framing.hop_size)
    if framing.fft_size < framing.frame_size:
        raise_para("fft_size 不得小于 frame_size", "framing.fft_size",
                   value=framing.fft_size)
    if framing.window_seconds <= 0 or framing.window_seconds > 3600:
        raise_para("window_seconds 须在 (0, 3600] 内", "framing.window_seconds",
                   value=framing.window_seconds)

    for index, stage in enumerate(project.stages):
        prefix = f"stages[{index}].config"
        cfg = stage.config
        if stage.kind is StageKind.SPECTRAL_SUBTRACTION and stage.enabled:
            strength = cfg.get("strength")
            if not isinstance(strength, (int, float)) or not (0.0 < strength <= 5.0):
                raise_para("谱减 strength 须在 (0, 5] 内", f"{prefix}.strength", value=strength)
            noise_frames = cfg.get("noise_frames", 8)
            if not isinstance(noise_frames, int) or noise_frames < 1:
                raise_para("noise_frames 须为 >=1 的整数", f"{prefix}.noise_frames",
                           value=noise_frames)
        if stage.kind is StageKind.MASK_SMOOTHING and stage.enabled:
            smoothing = cfg.get("smoothing", 0.5)
            if not (0.0 <= smoothing <= 1.0):
                raise_para("掩蔽 smoothing 须在 [0, 1] 内", f"{prefix}.smoothing",
                           value=smoothing)
            floor_db = cfg.get("floor_db", -12.0)
            if not (-100.0 <= floor_db <= 0.0):
                raise_para("floor_db 须在 [-100, 0] 内", f"{prefix}.floor_db", value=floor_db)
        if stage.kind is StageKind.NOISE_GATE and stage.enabled:
            threshold_db = cfg.get("threshold_db")
            if not isinstance(threshold_db, (int, float)) or not (-90.0 <= threshold_db <= 0.0):
                raise_para("门限 threshold_db 须在 [-90, 0] 内", f"{prefix}.threshold_db",
                           value=threshold_db)
            for key, bound in (("attack_ms", 1000.0), ("release_ms", 5000.0)):
                value = cfg.get(key, 0.0)
                if not (0.0 <= value <= bound):
                    raise_para(f"{key} 须在 [0, {bound}] 内", f"{prefix}.{key}", value=value)


@dataclass(frozen=True)
class WindowResult:
    """单窗口修复产物。"""

    samples: np.ndarray          # 修复采样 (n, channels) float64
    noise_spectrum: np.ndarray   # 噪声幅度谱估计 (bins,)
    content_hash: str            # 修复采样字节的 sha256
    sample_digest: str           # 同上，检查点字段命名用


def _hann(length: int) -> np.ndarray:
    # 对称 Hann，固定表达式，避免依赖库版本默认参数
    indices = np.arange(length, dtype=np.float64)
    return 0.5 - 0.5 * np.cos(2.0 * np.pi * indices / (length - 1))


def _stage(project: RestoreProject, kind: StageKind):
    for stage in project.stages:
        if stage.kind == kind:
            return stage
    return None


def process_window(samples: np.ndarray, project: RestoreProject) -> WindowResult:
    """对一个窗口的采样执行完整修复链，纯函数、无 RNG。

    :param samples: 形状 (n, channels) 的 float64 含噪采样
    """
    validate_params(project)
    frame_size = project.framing.frame_size
    hop_size = project.framing.hop_size
    fft_size = project.framing.fft_size
    n_samples, channels = samples.shape
    window = _hann(frame_size)
    bin_count = fft_size // 2 + 1

    sub_stage = _stage(project, StageKind.SPECTRAL_SUBTRACTION)
    smooth_stage = _stage(project, StageKind.MASK_SMOOTHING)
    gate_stage = _stage(project, StageKind.NOISE_GATE)

    strength = float(sub_stage.config["strength"]) if sub_stage and sub_stage.enabled else 0.0
    noise_frame_count = int(sub_stage.config.get("noise_frames", 8)) if sub_stage else 8
    smoothing = float(smooth_stage.config.get("smoothing", 0.5)) if smooth_stage else 0.0
    floor_gain = 10.0 ** (float(smooth_stage.config.get("floor_db", -12.0)) / 20.0) \
        if smooth_stage else 0.0
    gate_enabled = bool(gate_stage and gate_stage.enabled)
    threshold = 10.0 ** (float(gate_stage.config["threshold_db"]) / 20.0) if gate_enabled else 0.0

    # 首尾各补 frame_size//2 个零，保证每个原始采样都落在至少一帧的有效权重内，
    # 消除边界零权重除零放大；pad 长度固定，保持确定性。
    pad = frame_size // 2
    padded_input = np.zeros(n_samples + 2 * pad, dtype=np.float64)
    padded_input[pad:pad + n_samples] = samples[:, 0]
    total = padded_input.shape[0]
    starts = list(range(0, total - frame_size + 1, hop_size))
    if not starts:
        starts = [0]
    frame_count = len(starts)
    output = np.zeros(total, dtype=np.float64)
    weight = np.zeros(total, dtype=np.float64)
    noise_accum = np.zeros(bin_count, dtype=np.float64)

    # 预计算每帧幅度/相位
    magnitudes = np.zeros((frame_count, bin_count), dtype=np.float64)
    phases = np.zeros((frame_count, bin_count), dtype=np.float64)
    frame_energy = np.zeros(frame_count, dtype=np.float64)
    work = np.zeros(fft_size, dtype=np.float64)
    for frame_index, start in enumerate(starts):
        frame = padded_input[start:start + frame_size] * window
        work.fill(0.0)
        work[:frame_size] = frame
        spectrum = np.fft.rfft(work)
        magnitudes[frame_index] = np.abs(spectrum)
        phases[frame_index] = np.angle(spectrum)
        frame_energy[frame_index] = float(np.mean(frame ** 2))

    # 噪声谱估计：取窗口内能量最低的 noise_frame_count 帧做最小统计平均
    # （语音/音乐素材含静音段，低能量帧只含噪声），确定性排序无 RNG。
    estimate_count = min(noise_frame_count, frame_count)
    quietest = np.argsort(frame_energy, kind="stable")[:estimate_count]
    noise_spectrum = magnitudes[quietest].mean(axis=0)
    noise_accum += noise_spectrum

    # 谱减 + 掩蔽平滑（时间维一阶递归），再按帧逆变换 OLA。
    # mask 为逐频点增益（上限 1，只衰减不放大），配合 Hann 窗的
    # 标准 WOLA（weighted overlap-add）重建，确定性且不引入随机量。
    previous_mask = np.ones(bin_count, dtype=np.float64)
    previous_gain = 1.0
    hop_seconds = hop_size / project.audio.sample_rate
    attack_coef = np.exp(-hop_seconds / max(float(gate_stage.config.get("attack_ms", 5.0)) / 1000.0, 1e-6)) \
        if gate_enabled else 0.0
    release_coef = np.exp(-hop_seconds / max(float(gate_stage.config.get("release_ms", 50.0)) / 1000.0, 1e-6)) \
        if gate_enabled else 0.0

    for frame_index, start in enumerate(starts):
        magnitude = magnitudes[frame_index]
        if sub_stage and sub_stage.enabled:
            subtracted = magnitude - strength * noise_spectrum
            # 增益式谱减：低于噪声地板的频点保留 floor_gain 比例，避免音乐噪声
            rectified = np.maximum(subtracted, floor_gain * magnitude)
            raw_mask = rectified / np.maximum(magnitude, 1e-12)
            raw_mask = np.clip(raw_mask, 0.0, 1.0)
        else:
            raw_mask = np.ones_like(magnitude)
        if smooth_stage and smooth_stage.enabled:
            mask = smoothing * previous_mask + (1.0 - smoothing) * raw_mask
        else:
            mask = raw_mask
        previous_mask = mask
        processed = magnitude * mask * np.exp(1j * phases[frame_index])
        time_frame = np.fft.irfft(processed, n=fft_size)[:frame_size]

        gain = 1.0
        if gate_enabled:
            rms = float(np.sqrt(np.mean(time_frame ** 2) + 1e-20))
            target_gain = 1.0 if rms >= threshold else 0.0
            coef = attack_coef if target_gain > previous_gain else release_coef
            gain = coef * previous_gain + (1.0 - coef) * target_gain
            previous_gain = gain
        time_frame *= gain

        output[start:start + frame_size] += time_frame * window
        weight[start:start + frame_size] += window ** 2

    restored_ch0 = output / np.maximum(weight, 1e-8)
    restored_ch0 = restored_ch0[pad:pad + n_samples]
    restored = np.repeat(restored_ch0[:, None], channels, axis=1) if channels > 1 else restored_ch0[:, None]

    from .hashing import digest_samples
    content_hash = digest_samples(restored)
    return WindowResult(
        samples=restored,
        noise_spectrum=noise_accum,
        content_hash=content_hash,
        sample_digest=content_hash,
    )
