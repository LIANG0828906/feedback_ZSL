#pragma once
#include "vforensics/types.h"
#include <vector>

namespace vforensics {

struct CascadeResult {
  // Frames directly failing their content hash.
  std::vector<uint32_t> failureSources;
  // Every frame reachable from a failure source (including the source itself).
  std::vector<uint32_t> affectedFrames;
  // One concrete propagation path per affected frame, ending at that frame.
  std::vector<std::vector<uint32_t>> propagationPaths;
  // Human-readable edge descriptions, e.g. "3->5".
  std::vector<QString> affectedEdges;
};

class DependencyGraph {
public:
  // Validates that every reference is in range and that the graph is acyclic.
  // E_DEP_DANGLING / E_DEP_CYCLE are returned before any trust conclusion.
  static Error validate(const std::vector<std::vector<uint32_t>>& references);

  // Propagates failure along out-edges (edge frame -> referenced parent means
  // the parent's failure makes the dependent frame untrusted; the graph stores
  // child->parents, so propagation walks parent->children via the reverse map).
  static CascadeResult propagate(const std::vector<std::vector<uint32_t>>& references,
                                 const std::vector<uint32_t>& failureSources);

  // Returns the dependency path (child -> ... -> failed ancestor) for a frame.
  static std::vector<uint32_t> pathToFailure(
      const std::vector<std::vector<uint32_t>>& references,
      uint32_t frame,
      const std::vector<uint32_t>& failureSources);
};

} // namespace vforensics
