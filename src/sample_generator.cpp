#include "terrain/sample_generator.h"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "terrain/binary_io.h"
#include "terrain/hash.h"
#include "terrain/terrain_io.h"

namespace terrain {
namespace {

constexpr std::uint32_t kWidth = 129;
constexpr std::uint32_t kHeight = 129;
constexpr float kSpacing = 1.0f;
constexpr float kValidMin = -10.0f;
constexpr float kValidMax = 60.0f;

float baseHeight(std::uint32_t x, std::uint32_t y) {
    const double fx = static_cast<double>(x);
    const double fy = static_cast<double>(y);
    double h = 8.0 + 2.8 * std::sin(fx * 0.047) + 2.1 * std::cos(fy * 0.039);
    h += 3.5 * std::exp(-((fx - 35.0) * (fx - 35.0) +
                           (fy - 34.0) * (fy - 34.0)) /
                         720.0);
    h += 2.7 * std::exp(-((fx - 92.0) * (fx - 92.0) +
                           (fy - 84.0) * (fy - 84.0)) /
                         420.0);
    const double rough_a =
        ((x >= 20 && x < 48 && y >= 16 && y < 40) ||
         (x >= 72 && x < 104 && y >= 64 && y < 96))
            ? 1.0
            : 0.12;
    const double rough_b =
        (x >= 36 && x < 56 && y >= 36 && y < 56) ? 1.0 : 0.15;
    h += rough_a * (0.95 * std::sin(fx * 1.31) * std::cos(fy * 1.17) +
                    0.45 * std::sin(fx * 2.07 + fy * 0.73));
    h += rough_b * 0.55 * std::cos(fx * 1.83) * std::sin(fy * 1.57);
    return static_cast<float>(std::clamp(h, static_cast<double>(kValidMin),
                                         static_cast<double>(kValidMax)));
}

std::uint32_t baseMaterial(std::uint32_t x, std::uint32_t y) {
    const std::uint32_t value =
        (x / 13u + y / 17u + ((x * 7u + y * 11u) % 5u)) % 3u;
    return value;
}

std::vector<float> makeHeightPayload(const HeightField& base, const Rect& rect,
                                     std::uint64_t sequence) {
    std::vector<float> values;
    values.reserve(static_cast<std::size_t>(rect.width) * rect.height);
    for (std::uint32_t y = 0; y < rect.height; ++y) {
        for (std::uint32_t x = 0; x < rect.width; ++x) {
            const std::uint32_t gx = rect.x + x;
            const std::uint32_t gy = rect.y + y;
            double h = base.at(gx, gy);
            const double fx = static_cast<double>(gx);
            const double fy = static_cast<double>(gy);
            if (sequence == 1) {
                const double ridge =
                    std::sin(fx * 0.31) * std::cos(fy * 0.27) * 5.5;
                const double local = std::sin((fx - rect.x) * 0.8) *
                                     std::cos((fy - rect.y) * 0.7) * 2.2;
                h += ridge + local + 2.0;
            } else if (sequence == 2) {
                const double pit = -7.5 * std::exp(
                                              -((fx - 87.0) * (fx - 87.0) +
                                                (fy - 76.0) * (fy - 76.0)) /
                                              90.0);
                h += pit + 1.8 * std::sin(fx * 0.91) * std::cos(fy * 1.13);
            } else if (sequence == 4) {
                h += 3.4 * std::sin(fx * 1.7) * std::cos(fy * 1.3);
            }
            values.push_back(static_cast<float>(
                std::clamp(h, static_cast<double>(kValidMin),
                           static_cast<double>(kValidMax))));
        }
    }
    return values;
}

std::vector<std::uint32_t> makeMaterialPayload(const Rect& rect,
                                                std::uint32_t palette_size,
                                                std::uint64_t sequence) {
    std::vector<std::uint32_t> values;
    values.reserve(static_cast<std::size_t>(rect.width) * rect.height);
    for (std::uint32_t y = 0; y < rect.height; ++y) {
        for (std::uint32_t x = 0; x < rect.width; ++x) {
            const std::uint32_t gx = rect.x + x;
            const std::uint32_t gy = rect.y + y;
            values.push_back(
                (baseMaterial(gx, gy) + static_cast<std::uint32_t>(sequence)) %
                palette_size);
        }
    }
    return values;
}

Status saveSamplePatch(const std::string& path, const HeightField& base,
                       const Rect& rect, std::uint64_t baseline,
                       std::uint64_t sequence) {
    HeightPatch patch;
    patch.target = rect;
    patch.cell_spacing = base.cell_spacing;
    patch.height_type = base.height_type;
    patch.baseline_version = baseline;
    patch.sequence = sequence;
    patch.values = makeHeightPayload(base, rect, sequence);
    patch.payload_hash = toHex(hashFloats(patch.values));
    patch.source_path = path;
    return saveHeightPatch(path, patch);
}

Status saveSampleMaterialPatch(const std::string& path,
                               const MaterialImage& material,
                               const Rect& rect, std::uint64_t baseline,
                               std::uint64_t sequence) {
    MaterialPatch patch;
    patch.target = rect;
    patch.cell_spacing = material.cell_spacing;
    patch.channels = material.channels;
    patch.baseline_version = baseline;
    patch.sequence = sequence;
    patch.indices =
        makeMaterialPayload(rect, material.palette_size, sequence);
    patch.payload_hash = toHex(hashU32(patch.indices));
    patch.source_path = path;
    return saveMaterialPatch(path, patch);
}

}  // namespace

Status generateDeterministicSamples(const std::string& root,
                                    SamplePaths* paths) {
    std::string error;
    if (!makeDirectories(root, &error)) {
        return Status::error(E_IO, "samples", error);
    }
    const std::string patch_dir = joinPath(root, "patches");
    const std::string invalid_dir = joinPath(root, "invalid");
    const std::string store_dir = joinPath(root, "store");
    if (!makeDirectories(patch_dir, &error) ||
        !makeDirectories(invalid_dir, &error) ||
        !makeDirectories(store_dir, &error)) {
        return Status::error(E_IO, "samples", error);
    }

    HeightField base;
    base.width = kWidth;
    base.height = kHeight;
    base.cell_spacing = kSpacing;
    base.height_type = 0;
    base.valid_min = kValidMin;
    base.valid_max = kValidMax;
    base.baseline_version = 0;
    base.heights.resize(static_cast<std::size_t>(kWidth) * kHeight);
    for (std::uint32_t y = 0; y < kHeight; ++y) {
        for (std::uint32_t x = 0; x < kWidth; ++x) {
            base.at(x, y) = baseHeight(x, y);
        }
    }

    MaterialImage material;
    material.width = kWidth;
    material.height = kHeight;
    material.cell_spacing = kSpacing;
    material.channels = 1;
    material.palette_size = 8;
    material.baseline_version = 0;
    material.indices.resize(static_cast<std::size_t>(kWidth) * kHeight);
    for (std::uint32_t y = 0; y < kHeight; ++y) {
        for (std::uint32_t x = 0; x < kWidth; ++x) {
            material.at(x, y) = baseMaterial(x, y);
        }
    }

    const std::string height_path = joinPath(root, "height.bin");
    const std::string material_path = joinPath(root, "material.bin");
    Status status = saveHeightField(height_path, base);
    if (!status.ok()) return status;
    status = saveMaterialImage(material_path, material);
    if (!status.ok()) return status;

    const std::string p1h = joinPath(patch_dir, "0001_height.bin");
    const std::string p1m = joinPath(patch_dir, "0001_material.bin");
    const std::string p2h = joinPath(patch_dir, "0002_height.bin");
    const std::string p2m = joinPath(patch_dir, "0002_material.bin");
    const std::string p3m = joinPath(patch_dir, "0003_material.bin");
    const std::string p4h = joinPath(patch_dir, "0004_height.bin");
    status = saveSamplePatch(p1h, base, Rect{22, 18, 24, 20}, 0, 1);
    if (!status.ok()) return status;
    status = saveSampleMaterialPatch(p1m, material, Rect{20, 16, 28, 24}, 0,
                                     1);
    if (!status.ok()) return status;
    status = saveSamplePatch(p2h, base, Rect{78, 68, 18, 16}, 1, 2);
    if (!status.ok()) return status;
    status = saveSampleMaterialPatch(p2m, material, Rect{72, 62, 30, 26}, 1,
                                     2);
    if (!status.ok()) return status;
    status = saveSampleMaterialPatch(p3m, material, Rect{40, 60, 24, 24}, 2,
                                     3);
    if (!status.ok()) return status;
    status = saveSamplePatch(p4h, base, Rect{42, 42, 12, 12}, 3, 4);
    if (!status.ok()) return status;

    const std::string out_of_bounds =
        joinPath(invalid_dir, "out_of_bounds_height.bin");
    HeightPatch bounds_patch;
    bounds_patch.target = Rect{kWidth - 2, 2, 5, 5};
    bounds_patch.cell_spacing = kSpacing;
    bounds_patch.height_type = 0;
    bounds_patch.baseline_version = 0;
    bounds_patch.sequence = 1;
    bounds_patch.values.assign(25, 1.0f);
    bounds_patch.payload_hash = toHex(hashFloats(bounds_patch.values));
    bounds_patch.source_path = out_of_bounds;
    status = saveHeightPatch(out_of_bounds, bounds_patch);
    if (!status.ok()) return status;

    const std::string stale = joinPath(invalid_dir, "stale_height.bin");
    HeightPatch stale_patch;
    stale_patch.target = Rect{4, 4, 6, 6};
    stale_patch.cell_spacing = kSpacing;
    stale_patch.height_type = 0;
    stale_patch.baseline_version = 99;
    stale_patch.sequence = 100;
    stale_patch.values.assign(36, 2.0f);
    stale_patch.payload_hash = toHex(hashFloats(stale_patch.values));
    stale_patch.source_path = stale;
    status = saveHeightPatch(stale, stale_patch);
    if (!status.ok()) return status;

    if (paths != nullptr) {
        paths->root = root;
        paths->height_field = height_path;
        paths->material_image = material_path;
        paths->patch_directory = patch_dir;
        paths->height_patches = {p1h, p2h, p4h};
        paths->material_patches = {p1m, p2m, p3m};
        paths->out_of_bounds_patch = out_of_bounds;
        paths->stale_patch = stale;
        paths->store_directory = store_dir;
    }
    return Status::success();
}

}  // namespace terrain

