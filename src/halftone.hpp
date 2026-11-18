#pragma once

#include "math.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace sl {

struct HalftoneSample {
    double detail = 0.0;
    double smooth = 0.0;
    double edge = 0.0;

    double coverage(double filterWeight) const {
        return clamp(detail * (1.0 - filterWeight) + smooth * filterWeight, 0.0, 1.0);
    }
};

class HalftoneTable {
public:
    static constexpr int kSize = 256;
    static constexpr int kCellTexels = 32;
    static constexpr int kCells = kSize / kCellTexels;

    HalftoneTable() = default;
    static HalftoneTable make(std::uint64_t seed, int lineCount, double angleDegrees);

    std::uint64_t guardSeed() const { return seed_; }
    int lineCount() const { return lineCount_; }
    double angleDegrees() const { return angleDegrees_; }
    double phaseU() const { return phaseU_; }
    double phaseV() const { return phaseV_; }
    std::uint64_t digest() const { return digest_; }
    double meanCoverage() const { return meanCoverage_; }
    double meanAperture() const { return 1.0 - meanCoverage_; }

    HalftoneSample sampleWorld(const Vec3& point,
                               const Vec3& origin,
                               const Vec3& axisU,
                               const Vec3& axisV,
                               double referenceWidth,
                               double filterWeight) const;

    const std::vector<std::uint8_t>& detailBytes() const { return detail_; }
    const std::vector<std::uint8_t>& edgeBytes() const { return edge_; }
    void writeBinary(const std::string& path) const;

private:
    std::uint64_t seed_ = 0;
    int lineCount_ = 1;
    double angleDegrees_ = 0.0;
    double phaseU_ = 0.0;
    double phaseV_ = 0.0;
    double meanCoverage_ = 0.0;
    std::uint64_t digest_ = 0;
    std::vector<std::uint8_t> detail_;
    std::vector<std::uint8_t> smooth_;
    std::vector<std::uint8_t> edge_;

    double lookup(const std::vector<std::uint8_t>& table, double u, double v) const;
};

} // namespace sl

