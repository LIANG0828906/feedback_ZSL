#pragma once

#include "core/Error.h"
#include "core/Mesh.h"

#include <string>
#include <vector>

namespace meshedit {

struct ValidationConfig {
    double distanceTolerance = 1e-4;
    int samplingDensity = 4;
    double normalAngleToleranceDegrees = 45.0;
};

struct DeviationStats {
    std::size_t samples = 0;
    double minimum = 0.0;
    double maximum = 0.0;
    double mean = 0.0;
    double rms = 0.0;
    double p50 = 0.0;
    double p90 = 0.0;
    double p95 = 0.0;
    double p99 = 0.0;
};

struct ValidationReport {
    ValidationConfig config;
    MeshMetrics truthMetrics;
    MeshMetrics candidateMetrics;
    DeviationStats forward;
    DeviationStats reverse;
    DeviationStats combined;
    DeviationStats normalAngleDegrees;
    std::vector<int> violatingTruthFaces;
    std::vector<int> violatingCandidateFaces;
    bool metricsWithinTolerance = false;
    bool passed = false;

    std::string toJson() const;
    std::string toText() const;
};

Result<ValidationReport> validateMeshes(const Mesh& truth, const Mesh& candidate,
                                        const ValidationConfig& config);
Result<bool> writeValidationReport(const ValidationReport& report,
                                   const std::string& path);

} // namespace meshedit
