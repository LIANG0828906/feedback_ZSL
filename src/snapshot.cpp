#include "shadowpipe/snapshot.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <vector>

#include "shadowpipe/hash.hpp"

namespace shadowpipe {
namespace {

class SnapshotWriter {
public:
    void u8(std::uint8_t value) { data_.push_back(value); }
    void u32(std::uint32_t value) {
        for (int i = 0; i < 4; ++i) {
            data_.push_back(static_cast<std::uint8_t>((value >> (i * 8)) & 0xffu));
        }
    }
    void i32(std::int32_t value) { u32(static_cast<std::uint32_t>(value)); }
    void u64(std::uint64_t value) {
        u32(static_cast<std::uint32_t>(value & 0xffffffffULL));
        u32(static_cast<std::uint32_t>(value >> 32u));
    }
    void f32(float value) {
        std::uint32_t bits = 0;
        std::memcpy(&bits, &value, sizeof(bits));
        u32(bits);
    }
    void vec3(const Vec3& value) {
        f32(value.x);
        f32(value.y);
        f32(value.z);
    }
    void mat4(const Mat4& value) {
        for (int column = 0; column < 4; ++column) {
            for (int row = 0; row < 4; ++row) {
                f32(value[column][row]);
            }
        }
    }
    void frame(const LightFrame& value) {
        vec3(value.axis_x);
        vec3(value.axis_y);
        vec3(value.axis_z);
        f32(value.x_min);
        f32(value.x_max);
        f32(value.y_min);
        f32(value.y_max);
        f32(value.z_min);
        f32(value.z_max);
    }

    const std::vector<std::uint8_t>& data() const { return data_; }

private:
    std::vector<std::uint8_t> data_;
};

class SnapshotReader {
public:
    SnapshotReader(const std::vector<std::uint8_t>& data) : data_(data) {}

    bool u8(std::uint8_t& value) {
        if (offset_ >= data_.size()) {
            return false;
        }
        value = data_[offset_++];
        return true;
    }
    bool u32(std::uint32_t& value) {
        if (data_.size() - offset_ < 4) {
            return false;
        }
        value = static_cast<std::uint32_t>(data_[offset_]) |
                (static_cast<std::uint32_t>(data_[offset_ + 1]) << 8u) |
                (static_cast<std::uint32_t>(data_[offset_ + 2]) << 16u) |
                (static_cast<std::uint32_t>(data_[offset_ + 3]) << 24u);
        offset_ += 4;
        return true;
    }
    bool i32(std::int32_t& value) {
        std::uint32_t raw = 0;
        if (!u32(raw)) {
            return false;
        }
        value = static_cast<std::int32_t>(raw);
        return true;
    }
    bool u64(std::uint64_t& value) {
        std::uint32_t low = 0;
        std::uint32_t high = 0;
        if (!u32(low) || !u32(high)) {
            return false;
        }
        value = static_cast<std::uint64_t>(low) |
                (static_cast<std::uint64_t>(high) << 32u);
        return true;
    }
    bool f32(float& value) {
        std::uint32_t bits = 0;
        if (!u32(bits)) {
            return false;
        }
        std::memcpy(&value, &bits, sizeof(value));
        return true;
    }
    bool vec3(Vec3& value) {
        return f32(value.x) && f32(value.y) && f32(value.z);
    }
    bool mat4(Mat4& value) {
        for (int column = 0; column < 4; ++column) {
            for (int row = 0; row < 4; ++row) {
                if (!f32(value[column][row])) {
                    return false;
                }
            }
        }
        return true;
    }
    bool frame(LightFrame& value) {
        return vec3(value.axis_x) && vec3(value.axis_y) && vec3(value.axis_z) &&
               f32(value.x_min) && f32(value.x_max) && f32(value.y_min) &&
               f32(value.y_max) && f32(value.z_min) && f32(value.z_max);
    }
    bool done() const { return offset_ == data_.size(); }

private:
    const std::vector<std::uint8_t>& data_;
    std::size_t offset_ = 0;
};

void writeCascadeTable(SnapshotWriter& writer, const CascadeTable& table) {
    writer.vec3(table.camera_eye);
    writer.vec3(table.camera_forward);
    writer.vec3(table.camera_right);
    writer.vec3(table.camera_up);
    writer.mat4(table.view);
    writer.mat4(table.projection);
    writer.mat4(table.view_projection);
    writer.frame(table.scene_light_frame);
    writer.u32(static_cast<std::uint32_t>(table.cascades.size()));
    for (const CascadeInfo& cascade : table.cascades) {
        writer.u32(cascade.index);
        writer.f32(cascade.near_distance);
        writer.f32(cascade.far_distance);
        writer.u32(cascade.resolution_x);
        writer.u32(cascade.resolution_y);
        writer.f32(cascade.texel_world_size);
        writer.f32(cascade.depth_range);
        writer.f32(cascade.depth_quantization_step);
        writer.f32(cascade.max_depth_quantization_error);
        writer.frame(cascade.frame);
        writer.mat4(cascade.light_view_projection);
    }
    writer.u32(static_cast<std::uint32_t>(table.boundaries.size()));
    for (const CascadeBoundaryStats& boundary : table.boundaries) {
        writer.u32(boundary.near_cascade);
        writer.u32(boundary.far_cascade);
        writer.f32(boundary.near_texel_world_size);
        writer.f32(boundary.far_texel_world_size);
        writer.f32(boundary.texel_coverage_difference);
        writer.f32(boundary.relative_texel_coverage_difference);
    }
}

bool readCascadeTable(SnapshotReader& reader, CascadeTable& table) {
    if (!reader.vec3(table.camera_eye) || !reader.vec3(table.camera_forward) ||
        !reader.vec3(table.camera_right) || !reader.vec3(table.camera_up) ||
        !reader.mat4(table.view) || !reader.mat4(table.projection) ||
        !reader.mat4(table.view_projection) || !reader.frame(table.scene_light_frame)) {
        return false;
    }
    std::uint32_t count = 0;
    if (!reader.u32(count) || count > 4) {
        return false;
    }
    table.cascades.resize(count);
    for (CascadeInfo& cascade : table.cascades) {
        if (!reader.u32(cascade.index) || !reader.f32(cascade.near_distance) ||
            !reader.f32(cascade.far_distance) || !reader.u32(cascade.resolution_x) ||
            !reader.u32(cascade.resolution_y) ||
            !reader.f32(cascade.texel_world_size) ||
            !reader.f32(cascade.depth_range) ||
            !reader.f32(cascade.depth_quantization_step) ||
            !reader.f32(cascade.max_depth_quantization_error) ||
            !reader.frame(cascade.frame) ||
            !reader.mat4(cascade.light_view_projection)) {
            return false;
        }
    }
    if (!reader.u32(count) || count > 8) {
        return false;
    }
    table.boundaries.resize(count);
    for (CascadeBoundaryStats& boundary : table.boundaries) {
        if (!reader.u32(boundary.near_cascade) || !reader.u32(boundary.far_cascade) ||
            !reader.f32(boundary.near_texel_world_size) ||
            !reader.f32(boundary.far_texel_world_size) ||
            !reader.f32(boundary.texel_coverage_difference) ||
            !reader.f32(boundary.relative_texel_coverage_difference)) {
            return false;
        }
    }
    return true;
}

void writePyramidMetadata(SnapshotWriter& writer, const PyramidMetadata& metadata) {
    writer.frame(metadata.frame);
    writer.u32(static_cast<std::uint32_t>(metadata.levels.size()));
    for (const PyramidLevelMeta& level : metadata.levels) {
        writer.u32(level.index);
        writer.u32(level.width);
        writer.u32(level.height);
        writer.u32(level.quantization_bits);
        writer.f32(level.depth_min);
        writer.f32(level.depth_max);
        writer.f32(level.quantization_step);
        writer.f32(level.max_quantization_error);
    }
}

bool readPyramidMetadata(SnapshotReader& reader, PyramidMetadata& metadata) {
    if (!reader.frame(metadata.frame)) {
        return false;
    }
    std::uint32_t count = 0;
    if (!reader.u32(count) || count > 16) {
        return false;
    }
    metadata.levels.resize(count);
    for (PyramidLevelMeta& level : metadata.levels) {
        if (!reader.u32(level.index) || !reader.u32(level.width) ||
            !reader.u32(level.height) || !reader.u32(level.quantization_bits) ||
            !reader.f32(level.depth_min) || !reader.f32(level.depth_max) ||
            !reader.f32(level.quantization_step) ||
            !reader.f32(level.max_quantization_error)) {
            return false;
        }
    }
    return true;
}

void writeHashes(SnapshotWriter& writer, const ArtifactHashes& hashes) {
    writer.u64(hashes.shadow_factor_pgm);
    writer.u64(hashes.shadow_factor_f32);
    writer.u64(hashes.visibility_mask_pgm);
    writer.u64(hashes.error_heatmap_ppm);
    writer.u64(hashes.cascade_depth_maps);
    writer.u64(hashes.summary_json);
}

bool readHashes(SnapshotReader& reader, ArtifactHashes& hashes) {
    return reader.u64(hashes.shadow_factor_pgm) &&
           reader.u64(hashes.shadow_factor_f32) &&
           reader.u64(hashes.visibility_mask_pgm) &&
           reader.u64(hashes.error_heatmap_ppm) &&
           reader.u64(hashes.cascade_depth_maps) &&
           reader.u64(hashes.summary_json);
}

bool nearlyEqual(float a, float b) {
    if (a == b) {
        return true;
    }
    return std::abs(a - b) <=
           std::max(1.0e-7f, std::max(std::abs(a), std::abs(b)) * 2.0e-6f);
}

bool nearlyEqual(const Vec3& a, const Vec3& b) {
    return nearlyEqual(a.x, b.x) && nearlyEqual(a.y, b.y) && nearlyEqual(a.z, b.z);
}

bool cameraChanged(const CameraConfig& a, const CameraConfig& b) {
    return !nearlyEqual(a.eye, b.eye) || !nearlyEqual(a.target, b.target) ||
           !nearlyEqual(a.up, b.up) || !nearlyEqual(a.fov_y_deg, b.fov_y_deg) ||
           !nearlyEqual(a.near_plane, b.near_plane) ||
           !nearlyEqual(a.far_plane, b.far_plane);
}

bool matrixChanged(const Mat4& a, const Mat4& b) {
    for (int column = 0; column < 4; ++column) {
        for (int row = 0; row < 4; ++row) {
            if (!nearlyEqual(a[column][row], b[column][row])) {
                return true;
            }
        }
    }
    return false;
}

bool cascadeChanged(const CascadeTable& a, const CascadeTable& b) {
    if (a.cascades.size() != b.cascades.size()) {
        return true;
    }
    if (a.boundaries.size() != b.boundaries.size()) {
        return true;
    }
    for (std::size_t i = 0; i < a.cascades.size(); ++i) {
        const CascadeInfo& x = a.cascades[i];
        const CascadeInfo& y = b.cascades[i];
        if (x.index != y.index || !nearlyEqual(x.near_distance, y.near_distance) ||
            !nearlyEqual(x.far_distance, y.far_distance) ||
            x.resolution_x != y.resolution_x || x.resolution_y != y.resolution_y ||
            !nearlyEqual(x.texel_world_size, y.texel_world_size) ||
            !nearlyEqual(x.depth_range, y.depth_range) ||
            !nearlyEqual(x.frame.x_min, y.frame.x_min) ||
            !nearlyEqual(x.frame.x_max, y.frame.x_max) ||
            !nearlyEqual(x.frame.y_min, y.frame.y_min) ||
            !nearlyEqual(x.frame.y_max, y.frame.y_max) ||
            !nearlyEqual(x.frame.z_min, y.frame.z_min) ||
            !nearlyEqual(x.frame.z_max, y.frame.z_max) ||
            matrixChanged(x.light_view_projection, y.light_view_projection)) {
            return true;
        }
    }
    return false;
}

}  // namespace

Status writeSnapshotFile(const std::string& path, const SnapshotData& snapshot) {
    SnapshotWriter writer;
    writer.u32(snapshot.format_version);
    writer.u64(snapshot.scene_hash);
    writer.vec3(snapshot.camera.eye);
    writer.vec3(snapshot.camera.target);
    writer.vec3(snapshot.camera.up);
    writer.f32(snapshot.camera.fov_y_deg);
    writer.f32(snapshot.camera.near_plane);
    writer.f32(snapshot.camera.far_plane);
    writer.vec3(snapshot.light.direction);
    writeCascadeTable(writer, snapshot.cascade_table);
    writer.f32(snapshot.bias.depth_bias);
    writer.f32(snapshot.bias.normal_bias);
    writer.f32(snapshot.bias.slope_scale);
    writer.f32(snapshot.bias.max_bias_texels);
    writer.f32(snapshot.switch_tolerance);
    writePyramidMetadata(writer, snapshot.pyramid);
    writer.u32(snapshot.image_width);
    writer.u32(snapshot.image_height);
    writeHashes(writer, snapshot.hashes);

    const auto& payload = writer.data();
    const std::uint64_t payload_hash = hashBytes(payload.data(), payload.size());
    SnapshotWriter file_writer;
    file_writer.u8('R');
    file_writer.u8('S');
    file_writer.u8('N');
    file_writer.u8('P');
    file_writer.u32(kSnapshotFormatVersion);
    file_writer.u64(static_cast<std::uint64_t>(payload.size()));
    file_writer.u64(payload_hash);
    const auto& header = file_writer.data();

    std::vector<std::uint8_t> bytes;
    bytes.reserve(header.size() + payload.size());
    bytes.insert(bytes.end(), header.begin(), header.end());
    bytes.insert(bytes.end(), payload.begin(), payload.end());

    const std::string temporary = path + ".tmp";
    {
        std::ofstream out(temporary, std::ios::binary | std::ios::trunc);
        if (!out) {
            return Status::error(ErrorCode::IoError,
                                 "cannot create snapshot file: " + path);
        }
        out.write(reinterpret_cast<const char*>(bytes.data()),
                  static_cast<std::streamsize>(bytes.size()));
        out.flush();
        if (!out) {
            return Status::error(ErrorCode::IoError,
                                 "failed to write snapshot file: " + path);
        }
    }
    std::error_code ec;
    std::filesystem::rename(temporary, path, ec);
    if (ec) {
        // Windows cannot replace an existing file with rename. Move the previous
        // snapshot aside and restore it if the second rename fails.
        const std::string backup = path + ".bak";
        std::error_code backup_ec;
        std::filesystem::remove(backup, backup_ec);
        backup_ec.clear();
        std::filesystem::rename(path, backup, backup_ec);
        if (!backup_ec) {
            ec.clear();
            std::filesystem::rename(temporary, path, ec);
            if (ec) {
                std::error_code restore_ec;
                std::filesystem::rename(backup, path, restore_ec);
            } else {
                std::error_code remove_ec;
                std::filesystem::remove(backup, remove_ec);
            }
        }
    }
    if (ec) {
        std::filesystem::remove(temporary, ec);
        return Status::error(ErrorCode::IoError,
                             "cannot commit snapshot file: " + path);
    }
    return Status::success();
}

SnapshotLoadResult loadSnapshotFile(const std::string& path) {
    std::ifstream in(path, std::ios::binary | std::ios::ate);
    if (!in) {
        return {Status::error(ErrorCode::IoError,
                              "cannot open snapshot file: " + path),
                {}};
    }
    const std::streamoff file_size = in.tellg();
    if (file_size < 24 || file_size > 64LL * 1024LL * 1024LL) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot file size is invalid"),
                {}};
    }
    in.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> file(static_cast<std::size_t>(file_size));
    in.read(reinterpret_cast<char*>(file.data()), file_size);
    if (!in) {
        return {Status::error(ErrorCode::IoError,
                              "failed to read snapshot file: " + path),
                {}};
    }
    if (std::memcmp(file.data(), "RSNP", 4) != 0) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot magic mismatch"),
                {}};
    }
    SnapshotReader header_reader(file);
    for (int i = 0; i < 4; ++i) {
        std::uint8_t ignored = 0;
        header_reader.u8(ignored);
    }
    std::uint32_t version = 0;
    std::uint64_t payload_size = 0;
    std::uint64_t expected_hash = 0;
    if (!header_reader.u32(version) || version != kSnapshotFormatVersion ||
        !header_reader.u64(payload_size) || !header_reader.u64(expected_hash)) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot header is invalid or has an unsupported version"),
                {}};
    }
    if (payload_size != static_cast<std::uint64_t>(file.size() - 24)) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot payload size mismatch"),
                {}};
    }
    const std::uint64_t actual_hash =
        hashBytes(file.data() + 24, static_cast<std::size_t>(payload_size));
    if (actual_hash != expected_hash) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot content hash mismatch"),
                {}};
    }

    std::vector<std::uint8_t> payload(file.begin() + 24, file.end());
    SnapshotReader reader(payload);
    SnapshotData snapshot;
    if (!reader.u32(snapshot.format_version) || !reader.u64(snapshot.scene_hash) ||
        !reader.vec3(snapshot.camera.eye) || !reader.vec3(snapshot.camera.target) ||
        !reader.vec3(snapshot.camera.up) || !reader.f32(snapshot.camera.fov_y_deg) ||
        !reader.f32(snapshot.camera.near_plane) ||
        !reader.f32(snapshot.camera.far_plane) ||
        !reader.vec3(snapshot.light.direction) ||
        !readCascadeTable(reader, snapshot.cascade_table) ||
        !reader.f32(snapshot.bias.depth_bias) ||
        !reader.f32(snapshot.bias.normal_bias) ||
        !reader.f32(snapshot.bias.slope_scale) ||
        !reader.f32(snapshot.bias.max_bias_texels) ||
        !reader.f32(snapshot.switch_tolerance) ||
        !readPyramidMetadata(reader, snapshot.pyramid) ||
        !reader.u32(snapshot.image_width) || !reader.u32(snapshot.image_height) ||
        !readHashes(reader, snapshot.hashes) || !reader.done()) {
        return {Status::error(ErrorCode::SnapshotMismatch,
                              "snapshot payload is truncated or malformed"),
                {}};
    }
    return {Status::success(), std::move(snapshot)};
}

SnapshotVersionDiff compareSnapshotToFrame(const SnapshotData& previous,
                                           std::uint64_t scene_hash,
                                           const RenderConfig& config,
                                           const CascadeTable& cascade_table) {
    SnapshotVersionDiff diff;
    diff.scene_changed = previous.scene_hash != scene_hash;
    diff.view_changed = cameraChanged(previous.camera, config.camera);
    diff.light_changed =
        !nearlyEqual(previous.light.direction, config.light.direction);
    diff.cascade_table_changed =
        cascadeChanged(previous.cascade_table, cascade_table);
    diff.bias_changed =
        !nearlyEqual(previous.bias.depth_bias, config.depth_bias) ||
        !nearlyEqual(previous.bias.normal_bias, config.normal_bias) ||
        !nearlyEqual(previous.bias.slope_scale, config.slope_scale) ||
        !nearlyEqual(previous.bias.max_bias_texels, config.max_bias_texels) ||
        !nearlyEqual(previous.switch_tolerance, config.switch_tolerance);
    return diff;
}

}  // namespace shadowpipe

