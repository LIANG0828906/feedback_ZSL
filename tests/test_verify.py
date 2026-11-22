import os
import sqlite3

from caption_pipeline.engine import run_project
from verify import EXIT_OK, EXIT_SEG, EXIT_VIOLATION, verify_project


def _tamper(persist_dir, sql):
    conn = sqlite3.connect(os.path.join(persist_dir, "checkpoint.db"))
    with conn:
        conn.execute(sql)
    conn.close()


def test_verify_ok(manifest_path, tmp_path, capsys):
    persist = str(tmp_path / "ckpt")
    run_project(manifest_path, persist, workers=4)
    assert verify_project(manifest_path, persist) == EXIT_OK


def test_verify_detects_tampered_window(manifest_path, tmp_path):
    persist = str(tmp_path / "ckpt")
    run_project(manifest_path, persist, workers=4)
    _tamper(persist, "UPDATE lines SET win_end = win_end + 137"
                     " WHERE line_id = 'L003'")
    messages = []
    code = verify_project(manifest_path, persist, out=messages.append)
    assert code == EXIT_VIOLATION
    assert "L003" in messages[0] and "window" in messages[0]


def test_verify_e_seg_chain_and_retry(manifest_path, tmp_path):
    persist = str(tmp_path / "ckpt")
    run_project(manifest_path, persist, workers=4)
    # 注入坏段：把 L002 的窗口篡改成超出所在语音段 -> E_SEG，可重试
    _tamper(persist, "UPDATE lines SET win_start = 0"
                     " WHERE line_id = 'L002'")
    messages = []
    code = verify_project(manifest_path, persist, out=messages.append)
    assert code == EXIT_SEG
    assert "L002" in messages[0] and "E_SEG" in messages[0]
    # 修复后重试验证通过，其他台词未受污染
    _tamper(persist, "DELETE FROM lines WHERE line_id = 'L002'")
    run_project(manifest_path, persist, workers=4)  # 仅重算缺失台词
    assert verify_project(manifest_path, persist) == EXIT_OK


def test_verify_snapshot_version_mismatch(manifest_path, tmp_path):
    persist = str(tmp_path / "ckpt")
    run_project(manifest_path, persist, workers=4)
    _tamper(persist, "UPDATE meta SET value='pvX' WHERE key='param_version'")
    messages = []
    code = verify_project(manifest_path, persist, out=messages.append)
    assert code != EXIT_OK
    assert "INVALID_SNAPSHOT" in messages[0]
