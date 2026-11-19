import socket

from edge_sync.config import load_config
from edge_sync.supervisor import bring_up_stack


def _is_listening(host: str, port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.settimeout(0.3)
        return sock.connect_ex((host, port)) == 0


def test_ports_are_deterministic_and_released(running_stack):
    config, stack, _ = running_stack
    expected = {
        egress.egress_id: (egress.listen_host, egress.listen_port)
        for egress in config.egresses
    }
    for egress_id, address in expected.items():
        assert stack.proxy_states[egress_id].egress_id == egress_id
        assert _is_listening(*address)
    assert _is_listening(config.target_host, config.target_port)
    assert _is_listening(config.service_host, config.service_port)
    stack.shutdown()
    for address in list(expected.values()) + [
        (config.target_host, config.target_port),
        (config.service_host, config.service_port),
    ]:
        assert not _is_listening(*address)


def test_same_config_restarts_with_same_ports(workspace):
    from tests.conftest import _write_config

    path = _write_config(workspace, "restart")
    config = load_config(path)
    first = bring_up_stack(config)
    first_ports = {
        egress.egress_id: egress.listen_port for egress in config.egresses
    }
    first.shutdown()
    second = bring_up_stack(config)
    try:
        second_ports = {
            egress.egress_id: egress.listen_port for egress in config.egresses
        }
        assert first_ports == second_ports
    finally:
        second.shutdown()
