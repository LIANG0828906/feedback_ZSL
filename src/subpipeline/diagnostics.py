"""Headless diagnostic panel (no desktop environment required).

A Tk-based interactive panel is optional; :func:`render_report` produces the
same diagnostics as plain text/dict so the pipeline runs fully headless.
"""


def render_report(board, failed, run_dir):
    committed, total = board.progress
    lines = [
        "=== subtitle pipeline diagnostics (headless) ===",
        f"progress: {committed}/{total} committed",
        f"run dir:  {run_dir}",
        "states:",
    ]
    for sid in sorted(board.snapshot()):
        lines.append(f"  subtitle#{sid}: {board.snapshot()[sid]}")
    if failed:
        lines.append("failures:")
        for sid, exc in sorted(failed.items()):
            lines.append(f"  subtitle#{sid}: {exc}")
    else:
        lines.append("failures: none")
    return "\n".join(lines)


def try_gui_panel(board, failed, run_dir):
    """Best-effort desktop window; returns False when no display exists."""
    try:
        import tkinter as tk
    except Exception:
        return False
    try:
        root = tk.Tk()
        root.title("Subtitle pipeline diagnostics")
        text = tk.Text(root, width=72, height=24)
        text.pack()
        text.insert("1.0", render_report(board, failed, run_dir))
        text.configure(state="disabled")
        root.after(1200, root.destroy)
        root.mainloop()
        return True
    except Exception:
        return False
