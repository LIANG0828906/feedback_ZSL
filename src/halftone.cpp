#include "halftone.hpp"

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <stdexcept>

namespace sl {
namespace {

std::uint64_t angleKey(double angleDegrees) {
    double normalized = std::fmod(angleDegrees, 180.0);
    if (normalized < 0.0) normalized += 180.0;
    return static_cast<std::uint64_t>(std::llround(normalized * 10000.0));
}

double wrapCoord(double value, double period) {
    double result = std::fmod(value, period);
    if (result < 0.0) result += period;
    return result;
}

} // namespace

HalftoneTable HalftoneTable::make(std::uint64_t seed, int lineCount, double angleDegrees) {
    HalftoneTable table;
    table.seed_ = seed;
    table.lineCount_ = lineCount;
    table.angleDegrees_ = angleDegrees;

    const std::uint64_t akey = angleKey(angleDegrees);
    const std::uint64_t phaseHash = hashCombine(hashCombine(seed, akey), 0x50484153ULL);
    table.phaseU_ = static_cast<double>(phaseHash & 0xFFFFFFULL) / 16777216.0;
    table.phaseV_ = static_cast<double>((phaseHash >> 24u) & 0xFFFFFFULL) / 16777216.0;

    table.detail_.assign(static_cast<std::size_t>(kSize * kSize), 0);
    table.edge_.assign(static_cast<std::size_t>(kSize * kSize), 0);
    table.smooth_.assign(static_cast<std::size_t>(kSize * kSize), 0);

    const double baseRadius = std::sqrt(0.48 / kPi);
    std::uint64_t byteHash = 1469598103934665603ULL;
    for (int cy = 0; cy < kCells; ++cy) {
        for (int cx = 0; cx < kCells; ++cx) {
            const std::uint64_t cellIndex =
                static_cast<std::uint64_t>(cy * kCells + cx);
            const std::uint64_t h =
                hashCombine(hashCombine(hashCombine(seed, akey), cellIndex),
                            static_cast<std::uint64_t>(lineCount));
            const double jitterX = (static_cast<double>((h >> 8u) & 0xFFFFULL) / 65535.0 - 0.5) * 0.18;
            const double jitterY = (static_cast<double>((h >> 24u) & 0xFFFFULL) / 65535.0 - 0.5) * 0.18;
            const double radius = baseRadius * (0.88 + 0.18 * static_cast<double>((h >> 40u) & 0xFFULL) / 255.0);
            const double edgePhase = static_cast<double>((h >> 48u) & 0x7FULL) / 127.0 * 6.283185307179586;

            for (int sy = 0; sy < kCellTexels; ++sy) {
                for (int sx = 0; sx < kCellTexels; ++sx) {
                    const double fx = (static_cast<double>(sx) + 0.5) / kCellTexels - 0.5;
                    const double fy = (static_cast<double>(sy) + 0.5) / kCellTexels - 0.5;
                    const double dx = fx - jitterX;
                    const double dy = fy - jitterY;
                    const double distance = std::sqrt(dx * dx + dy * dy);
                    const double angular = std::atan2(dy, dx);
                    const double wobble = 0.018 * std::sin(angular * 5.0 + edgePhase);
                    const bool ink = distance + wobble < radius;
                    const int x = cx * kCellTexels + sx;
                    const int y = cy * kCellTexels + sy;
                    const std::size_t index = static_cast<std::size_t>(y * kSize + x);
                    table.detail_[index] = ink ? 255 : 0;

                    const std::uint64_t eh =
                        hashCombine(h, static_cast<std::uint64_t>(y * kSize + x));
                    const std::uint8_t edgeValue = static_cast<std::uint8_t>(eh & 0xFFULL);
                    table.edge_[index] = edgeValue;
                    byteHash ^= table.detail_[index];
                    byteHash *= 1099511628211ULL;
                    byteHash ^= edgeValue;
                    byteHash *= 1099511628211ULL;
                }
            }
        }
    }

    const int blurRadius = 4;
    for (int y = 0; y < kSize; ++y) {
        for (int x = 0; x < kSize; ++x) {
            int sum = 0;
            int count = 0;
            for (int oy = -blurRadius; oy <= blurRadius; ++oy) {
                for (int ox = -blurRadius; ox <= blurRadius; ++ox) {
                    int px = (x + ox + kSize) % kSize;
                    int py = (y + oy + kSize) % kSize;
                    sum += table.detail_[static_cast<std::size_t>(py * kSize + px)];
                    ++count;
                }
            }
            table.smooth_[static_cast<std::size_t>(y * kSize + x)] =
                static_cast<std::uint8_t>((sum + count / 2) / count);
        }
    }

    double sum = 0.0;
    for (std::uint8_t value : table.detail_) {
        sum += value == 0 ? 0.0 : 1.0;
    }
    table.meanCoverage_ = sum / static_cast<double>(table.detail_.size());
    table.digest_ = byteHash;
    return table;
}

double HalftoneTable::lookup(const std::vector<std::uint8_t>& table, double u, double v) const {
    const double x = wrapCoord(u, static_cast<double>(kSize));
    const double y = wrapCoord(v, static_cast<double>(kSize));
    const int x0 = static_cast<int>(std::floor(x)) % kSize;
    const int y0 = static_cast<int>(std::floor(y)) % kSize;
    const int x1 = (x0 + 1) % kSize;
    const int y1 = (y0 + 1) % kSize;
    const double fx = x - std::floor(x);
    const double fy = y - std::floor(y);

    const auto at = [&](int px, int py) -> double {
        return static_cast<double>(table[static_cast<std::size_t>((py + kSize) % kSize * kSize +
                                                                  (px + kSize) % kSize)]) /
               255.0;
    };
    const double a = at(x0, y0) * (1.0 - fx) + at(x1, y0) * fx;
    const double b = at(x0, y1) * (1.0 - fx) + at(x1, y1) * fx;
    return a * (1.0 - fy) + b * fy;
}

HalftoneSample HalftoneTable::sampleWorld(const Vec3& point,
                                          const Vec3& origin,
                                          const Vec3& axisU,
                                          const Vec3& axisV,
                                          double referenceWidth,
                                          double filterWeight) const {
    (void)filterWeight;
    const double spacing = std::max(referenceWidth / static_cast<double>(std::max(1, lineCount_)),
                                    1.0e-6);
    const double gu = dot(point - origin, axisU) / spacing + phaseU_;
    const double gv = dot(point - origin, axisV) / spacing + phaseV_;

    const double tx = wrapCoord(gu, static_cast<double>(kCells)) * kCellTexels;
    const double ty = wrapCoord(gv, static_cast<double>(kCells)) * kCellTexels;
    HalftoneSample sample;
    sample.detail = lookup(detail_, tx, ty);
    sample.smooth = lookup(smooth_, tx, ty);
    sample.edge = lookup(edge_, tx + 17.0, ty + 29.0);
    sample.detail = clamp(sample.detail + (sample.edge - 0.5) * 0.06, 0.0, 1.0);
    return sample;
}

void HalftoneTable::writeBinary(const std::string& path) const {
    std::filesystem::path target(path);
    if (!target.parent_path().empty()) {
        std::filesystem::create_directories(target.parent_path());
    }
    std::ofstream out(target, std::ios::binary);
    if (!out) {
        throw std::runtime_error("cannot open halftone table output: " + path);
    }
    const char magic[8] = {'S', 'L', 'H', 'T', '0', '0', '0', '1'};
    out.write(magic, sizeof(magic));
    out.write(reinterpret_cast<const char*>(&seed_), sizeof(seed_));
    out.write(reinterpret_cast<const char*>(&lineCount_), sizeof(lineCount_));
    out.write(reinterpret_cast<const char*>(&angleDegrees_), sizeof(angleDegrees_));
    out.write(reinterpret_cast<const char*>(&phaseU_), sizeof(phaseU_));
    out.write(reinterpret_cast<const char*>(&phaseV_), sizeof(phaseV_));
    out.write(reinterpret_cast<const char*>(detail_.data()),
              static_cast<std::streamsize>(detail_.size()));
    out.write(reinterpret_cast<const char*>(edge_.data()),
              static_cast<std::streamsize>(edge_.size()));
}

} // namespace sl

