from __future__ import annotations

try:
    from PySide6 import QtCore, QtGui, QtWidgets

    QT_API = "PySide6"
    Signal = QtCore.Signal
    Slot = QtCore.Slot
except ImportError:
    try:
        from PyQt5 import QtCore, QtGui, QtWidgets

        QT_API = "PyQt5"
        Signal = QtCore.pyqtSignal
        Slot = QtCore.pyqtSlot
    except ImportError as exc:
        raise ImportError(
            "PySide6 is required for the desktop workbench. "
            "Install the local PySide6 wheel or run the headless CLI/tests."
        ) from exc


def exec_application(app: object) -> int:
    if hasattr(app, "exec"):
        return int(app.exec())
    return int(app.exec_())


def exec_dialog(dialog: object) -> int:
    if hasattr(dialog, "exec"):
        return int(dialog.exec())
    return int(dialog.exec_())


def resize_to_contents() -> object:
    return getattr(QtWidgets.QHeaderView, "ResizeToContents", QtWidgets.QHeaderView.ResizeMode.ResizeToContents)


def stretch_section() -> object:
    return getattr(QtWidgets.QHeaderView, "Stretch", QtWidgets.QHeaderView.ResizeMode.Stretch)


def select_rows() -> object:
    return getattr(
        QtWidgets.QAbstractItemView,
        "SelectRows",
        QtWidgets.QAbstractItemView.SelectionBehavior.SelectRows,
    )


def extended_selection() -> object:
    return getattr(
        QtWidgets.QAbstractItemView,
        "ExtendedSelection",
        QtWidgets.QAbstractItemView.SelectionMode.ExtendedSelection,
    )


def no_edit_triggers() -> object:
    return getattr(
        QtWidgets.QAbstractItemView,
        "NoEditTriggers",
        QtWidgets.QAbstractItemView.EditTrigger.NoEditTriggers,
    )
