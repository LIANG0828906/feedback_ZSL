# Local persistence

This directory is writable by the local runtime account and stores:

- `checkpoints/<glyph>/<content-and-parameter-hash>/manifest.json`: atomic snapshot manifest.
- `checkpoints/<glyph>/<identity>/segment_*.npz`: flattened points and raster components.
- `checkpoints/<glyph>/<identity>/states.json`: persisted five-state segment matrix.
- `runtime/glyph_model.json`: editable model copy used by the desktop workbench.

Checkpoint updates are serialized with a process-wide lock plus `flock`, written to a
temporary file in the same directory, fsynced, and atomically replaced. A crash can
leave an unreferenced segment file, but the manifest never references a partial write.

