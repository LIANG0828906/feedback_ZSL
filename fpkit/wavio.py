"""16-bit PCM WAV parsing with field-located unsupported errors."""

import wave

import numpy as np

from .errors import AudioUnsupported

MIN_RATE = 8000
MAX_RATE = 48000
SUPPORTED_CHANNELS = (1, 2)
SUPPORTED_SAMPWIDTH = 2  # 16-bit PCM


def read_wav(path):
    """Read a WAV file -> (mono float64 signal in [-1,1], sample_rate).

    Raises AudioUnsupported (E_AUDIO_UNSUPPORTED) with the offending field
    located when the layout is not supported.
    """
    try:
        wf = wave.open(str(path), "rb")
    except (wave.Error, EOFError) as exc:
        raise AudioUnsupported(f"cannot parse RIFF/WAVE: {exc}", field="riff")
    with wf:
        channels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        rate = wf.getframerate()
        comptype = wf.getcomptype()
        if comptype != "NONE":
            raise AudioUnsupported(f"compressed wav: {comptype}", field="compression")
        if sampwidth != SUPPORTED_SAMPWIDTH:
            raise AudioUnsupported(
                f"bits_per_sample={sampwidth * 8}, need 16", field="bits_per_sample"
            )
        if channels not in SUPPORTED_CHANNELS:
            raise AudioUnsupported(f"channels={channels}, need 1 or 2", field="channels")
        if not (MIN_RATE <= rate <= MAX_RATE):
            raise AudioUnsupported(
                f"sample_rate={rate}, need {MIN_RATE}..{MAX_RATE}", field="sample_rate"
            )
        raw = wf.readframes(wf.getnframes())
    data = np.frombuffer(raw, dtype="<i2")
    if channels == 2:
        data = data.reshape(-1, 2)
        mono = data.astype(np.float64).mean(axis=1)
    else:
        mono = data.astype(np.float64)
    return mono / 32768.0, rate
