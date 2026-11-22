# Offline wheel directory

Place the CPython 3.11 Windows (or Linux) wheels used by this project here. The
only required runtime wheel is NumPy:

```text
numpy-1.26.4-cp311-cp311-win_amd64.whl     # Windows
numpy-1.26.4-cp311-cp311-...linux...whl    # Linux, if needed
```

Install fully offline:

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install --no-index --find-links=wheelhouse -r requirements.txt
```

Tkinter ships with the python.org Windows installer and is not a wheel.
PyOpenGL is optional because the delivered rasterizer is a deterministic NumPy
CPU implementation.

