import struct

import numpy as np
import pytest

from loudmeter.errors import WavInvalidError
from loudmeter.samplegen import generate_all, write_wav
from loudmeter.wavfile import read_wav, downmix


@pytest.fixture(scope="module")
def assets(tmp_path_factory):
    d = tmp_path_factory.mktemp("samples")
    return generate_all(d)


def test_read_16_and_24_bit(assets):
    for a in assets:
        w = read_wav(a["path"])
        assert w.sample_rate == 48000
        assert w.bits_per_sample == a["bits"]
        assert w.samples.shape[1] == 6
        assert np.abs(w.samples).max() <= 1.0


def test_downmix_weights(assets):
    w = read_wav(assets[0]["path"])
    mono = downmix(w.samples, (1, 1, 1, 0, 1, 1))
    assert mono.shape == (w.n_frames,)
    np.testing.assert_allclose(mono, w.samples[:, :5].sum(axis=1) * 1.0, atol=1e-9)


def _corrupt(path, offset, fmt, value):
    raw = bytearray(open(path, "rb").read())
    struct.pack_into(fmt, raw, offset, value)
    open(path, "wb").write(bytes(raw))


def test_invalid_rate(tmp_path, assets):
    p = tmp_path / "bad.wav"
    p.write_bytes(open(assets[0]["path"], "rb").read())
    _corrupt(p, 24, "<I", 44100)
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_invalid_channels(tmp_path, assets):
    p = tmp_path / "bad.wav"
    p.write_bytes(open(assets[0]["path"], "rb").read())
    _corrupt(p, 22, "<H", 2)
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_invalid_bits(tmp_path, assets):
    p = tmp_path / "bad.wav"
    p.write_bytes(open(assets[0]["path"], "rb").read())
    _corrupt(p, 34, "<H", 32)
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_invalid_byte_rate(tmp_path, assets):
    p = tmp_path / "bad.wav"
    p.write_bytes(open(assets[0]["path"], "rb").read())
    _corrupt(p, 28, "<I", 12345)
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_invalid_channel_mask(tmp_path, assets):
    p = tmp_path / "bad.wav"
    p.write_bytes(open(assets[0]["path"], "rb").read())
    _corrupt(p, 12 + 8 + 20, "<I", 0x0003)  # stereo mask only
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_truncated_data(tmp_path, assets):
    p = tmp_path / "bad.wav"
    raw = open(assets[0]["path"], "rb").read()
    p.write_bytes(raw[:-7])  # data no longer a whole number of frames
    with pytest.raises(WavInvalidError):
        read_wav(str(p))


def test_not_a_wav(tmp_path):
    p = tmp_path / "bad.wav"
    p.write_bytes(b"NOPE" * 32)
    with pytest.raises(WavInvalidError):
        read_wav(str(p))
