#pragma once

#include "core/Format.h"

namespace meshedit {

// Compatibility boundary used by every load/export path. The internal mesh model
// is always v2; older files are upgraded before they become visible to the editor.
Result<ConversionReport> upgradeLoadedMesh(Mesh& mesh, MeshFormat format, int sourceVersion);

// Returns E_VERSION_LOSS when the requested target cannot preserve attributes.
// Format-only losses are returned in the report without blocking the export.
Result<ConversionReport> checkExportCompatibility(const Mesh& mesh,
                                                  const ExportOptions& options);

} // namespace meshedit
