#pragma once
#include "vforensics/types.h"
#include <QString>
#include <optional>

namespace vforensics {

class Manifest {
public:
  // Parses a manifest. When previousVersion is provided and differs from the
  // new manifest version, anchor semantics are compared and diffs returned.
  // E_MANIFEST_VERSION is returned when a newer manifest omits fields that
  // would otherwise be inferred from the old version (no silent migration).
  static Error parse(const QString& path,
                     std::optional<uint32_t> expectedFrameCount,
                     ManifestData& out,
                     std::vector<AnchorDiff>* diffs = nullptr,
                     const ManifestData* previous = nullptr);

  static std::vector<AnchorDiff> diffAnchors(const ManifestData& oldM,
                                             const ManifestData& newM);
};

} // namespace vforensics
