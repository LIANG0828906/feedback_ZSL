import struct


def test_windows_tcp_info_v0_layout_constants():
    # These constants are exercised through ctypes on real Windows only, but
    # the layout can be validated offline so a refactor cannot silently break
    # the platform backend.
    from edge_sync import tcp_probe

    assert tcp_probe._SIO_TCP_INFO == 0x18000077
    size = struct.calcsize(tcp_probe._WIN_TCP_INFO_V0)
    assert size == 96


def test_windows_struct_field_offsets():
    from edge_sync import tcp_probe

    # State(0), StateV1(4), RttUs(8), RttVarUs(12), RetransBytes(16)...
    fmt = tcp_probe._WIN_TCP_INFO_V0
    # Build a zeroed 96-byte payload and confirm named field decoding.
    raw = bytearray(96)
    struct.pack_into("<I", raw, 12, 12345)     # RttUs (index 3)
    struct.pack_into("<I", raw, 36, 77)        # ReXmitCount (index 9)
    struct.pack_into("<I", raw, 4, 1460)       # Mss (index 1)
    struct.pack_into("<I", raw, 80, 2920)      # BytesInFlight (index 17)
    values = struct.unpack(fmt, bytes(raw))
    assert values[3] == 12345
    assert values[9] == 77
    assert values[1] == 1460
    assert values[17] == 2920


def test_linux_probe_marks_unsupported_platform(monkeypatch):
    import edge_sync.tcp_probe as tcp_probe
    from edge_sync import ERR_SIGNAL_UNSUPPORTED

    monkeypatch.setattr(tcp_probe.sys, "platform", "darwin")
    signals = tcp_probe.probe(object())
    assert signals.rtt_ms is None
    assert ERR_SIGNAL_UNSUPPORTED in signals.missing_codes
