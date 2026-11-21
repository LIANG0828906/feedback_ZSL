# Versioned interoperability examples

- `marker_description_v1.json`: legacy marker-only description.
- `sync_report_v1.json`: v1 export; only marker arrival samples survive.
- `sync_report_v2.json`: v2 export with segment delays and first-order drift models.
- `sync_report_v3.json`: full v3 report with channel layout, compensation parameters,
  residual summary, provenance, issues and content hashes.
- `baseline_residual.json`: synthetic-generator truth baseline.

The v1 and v2 files include a `compatibility` object whose `code` is
`E_VERSION_LOSS` and whose `lost_fields` array lists every field removed by the
downgrade. Loading any of these files through `mcsync.versioning.load_report`
produces the same internal v3 representation and records field provenance.

