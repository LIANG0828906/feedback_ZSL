#include "terrain/terrain_io.h"

#include <cmath>
#include <cstring>
#include <limits>
#include <utility>

#include "terrain/binary_io.h"
#include "terrain/hash.h"

namespace terrain {
namespace {

constexpr std::uint64_t kMaxCells = 200000000ull;
constexpr std::uint32_t kMaxDimension = 65535u;

Status invalid(std::string field, std::string message) {
    return Status::error(E_HEIGHT_INVALID, std::move(field),
                         std::move(message));
}

bool finiteFloat(float value) {
    return std::isfinite(static_cast<double>(value));
}

bool readHeaderPrefix(ByteReader* reader, const char* expected_magic,
                      std::uint32_t expected_header_size,
                      const std::string& source, std::string* field,
                      std::string* message) {
    char magic[4]{};
    if (!reader->bytes(magic, 4)) {
        *field = "magic";
        *message = source + ": truncated magic";
        return false;
    }
    if (std::memcmp(magic, expected_magic, 4) != 0) {
        *field = "magic";
        *message = source + ": unexpected magic";
        return false;
    }
    std::uint32_t header_size = 0;
    if (!reader->u32(&header_size)) {
        *field = "header_size";
        *message = source + ": truncated header_size";
        return false;
    }
    if (header_size != expected_header_size) {
        *field = "header_size";
        *message = source + ": expected " +
                   std::to_string(expected_header_size) + ", got " +
                   std::to_string(header_size);
        return false;
    }
    std::uint32_t format = 0;
    if (!reader->u32(&format)) {
        *field = "format";
        *message = source + ": truncated format";
        return false;
    }
    if (format != 1) {
        *field = "format";
        *message = source + ": unsupported format " + std::to_string(format);
        return false;
    }
    return true;
}

Status writeBinaryFile(const std::string& path,
                       const std::vector<std::uint8_t>& bytes) {
    std::string error;
    if (!writeFileAtomic(path, bytes, &error)) {
        return Status::error(E_IO, "path", error);
    }
    return Status::success();
}

}  // namespace

std::string computeHeightHash(const HeightField& field) {
    return toHex(hashFloats(field.heights));
}

std::string computeMaterialHash(const MaterialImage& material) {
    return toHex(hashU32(material.indices));
}

std::string computeBatchHash(const PatchBatch& batch) {
    ByteWriter writer;
    writer.u64(batch.sequence);
    if (batch.height) {
        writer.u32(1);
        writer.u32(batch.height->target.x);
        writer.u32(batch.height->target.y);
        writer.u32(batch.height->target.width);
        writer.u32(batch.height->target.height);
        writer.f32(batch.height->cell_spacing);
        writer.u32(batch.height->height_type);
        writer.u64(batch.height->baseline_version);
        writer.u64(batch.height->sequence);
        writer.bytes(batch.height->payload_hash.data(),
                     batch.height->payload_hash.size());
    } else {
        writer.u32(0);
    }
    if (batch.material) {
        writer.u32(2);
        writer.u32(batch.material->target.x);
        writer.u32(batch.material->target.y);
        writer.u32(batch.material->target.width);
        writer.u32(batch.material->target.height);
        writer.f32(batch.material->cell_spacing);
        writer.u32(batch.material->channels);
        writer.u64(batch.material->baseline_version);
        writer.u64(batch.material->sequence);
        writer.bytes(batch.material->payload_hash.data(),
                     batch.material->payload_hash.size());
    } else {
        writer.u32(0);
    }
    return toHex(sha256(writer.data()));
}

std::vector<std::uint8_t> encodeHeightField(const HeightField& field) {
    ByteWriter writer;
    writer.bytes("THF1", 4);
    writer.u32(kHeightHeaderSize);
    writer.u32(1);
    writer.u32(field.width);
    writer.u32(field.height);
    writer.f32(field.cell_spacing);
    writer.u32(field.height_type);
    writer.f32(field.valid_min);
    writer.f32(field.valid_max);
    writer.u32(0);
    writer.u64(field.baseline_version);
    const auto payload = hashFloats(field.heights);
    const std::uint64_t payload_bytes =
        static_cast<std::uint64_t>(field.heights.size()) * 4ull;
    writer.u64(payload_bytes);
    writer.bytes(payload.data(), payload.size());
    writer.u32(0);
    for (float value : field.heights) {
        writer.f32(value);
    }
    return writer.data();
}

Status parseHeightField(const std::vector<std::uint8_t>& bytes,
                        const std::string& source, HeightField* out) {
    ByteReader reader(bytes.data(), bytes.size());
    std::string field;
    std::string message;
    if (!readHeaderPrefix(&reader, "THF1", kHeightHeaderSize, source, &field,
                          &message)) {
        return invalid(field, message);
    }
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    float spacing = 0.0f;
    std::uint32_t height_type = 0;
    float valid_min = 0.0f;
    float valid_max = 0.0f;
    std::uint32_t flags = 0;
    std::uint64_t baseline_version = 0;
    std::uint64_t payload_bytes = 0;
    Digest payload_hash{};
    std::uint32_t reserved = 0;
    if (!reader.u32(&width) || !reader.u32(&height) ||
        !reader.f32(&spacing) || !reader.u32(&height_type) ||
        !reader.f32(&valid_min) || !reader.f32(&valid_max) ||
        !reader.u32(&flags) || !reader.u64(&baseline_version) ||
        !reader.u64(&payload_bytes) ||
        !reader.bytes(payload_hash.data(), payload_hash.size()) ||
        !reader.u32(&reserved)) {
        return invalid("header", source + ": truncated header");
    }
    if (reserved != 0) {
        return invalid("reserved", source + ": reserved field must be zero");
    }
    if (width < 2) {
        return invalid("width", source + ": width must be >= 2");
    }
    if (height < 2) {
        return invalid("height", source + ": height must be >= 2");
    }
    if (width > kMaxDimension || height > kMaxDimension) {
        return invalid("width",
                       source + ": dimensions exceed 65535 block-id limit");
    }
    if (static_cast<std::uint64_t>(width) * height > kMaxCells) {
        return invalid("width", source + ": dimensions exceed safety limit");
    }
    if (!finiteFloat(spacing) || spacing <= 0.0f) {
        return invalid("cell_spacing",
                       source + ": cell spacing must be finite and positive");
    }
    if (height_type != 0) {
        return invalid("height_type",
                       source + ": only float32 (0) is supported");
    }
    if (!finiteFloat(valid_min) || !finiteFloat(valid_max)) {
        return invalid("valid_range",
                       source + ": valid range must be finite");
    }
    if (valid_min > valid_max) {
        return invalid("valid_range",
                       source + ": valid_min exceeds valid_max");
    }
    if (flags != 0) {
        return invalid("flags", source + ": unsupported flags");
    }
    const std::uint64_t expected_bytes =
        static_cast<std::uint64_t>(width) * height * 4ull;
    if (payload_bytes != expected_bytes) {
        return invalid("payload_bytes",
                       source + ": expected " + std::to_string(expected_bytes) +
                           ", got " + std::to_string(payload_bytes));
    }
    if (reader.remaining() != expected_bytes) {
        return invalid("payload_bytes",
                       source + ": payload length does not match header");
    }
    const Digest actual_hash = sha256(reader.current(), reader.remaining());
    if (actual_hash != payload_hash) {
        return invalid("payload_hash",
                       source + ": declared content hash does not match data");
    }
    HeightField parsed;
    parsed.width = width;
    parsed.height = height;
    parsed.cell_spacing = spacing;
    parsed.height_type = height_type;
    parsed.valid_min = valid_min;
    parsed.valid_max = valid_max;
    parsed.baseline_version = baseline_version;
    parsed.payload_hash = toHex(payload_hash);
    parsed.heights.resize(static_cast<std::size_t>(width) * height);
    const std::uint8_t* payload = reader.current();
    for (std::uint32_t y = 0; y < height; ++y) {
        for (std::uint32_t x = 0; x < width; ++x) {
            const std::size_t idx = static_cast<std::size_t>(y) * width + x;
            float value = 0.0f;
            if (!readLittleF32(payload, reader.remaining(), idx * 4u, &value)) {
                return invalid("payload_bytes",
                               source + ": truncated elevation payload");
            }
            if (!std::isfinite(value)) {
                return invalid("height[" + std::to_string(x) + "," +
                                   std::to_string(y) + "]",
                               source + ": elevation is not finite");
            }
            if (value < valid_min || value > valid_max) {
                return invalid("valid_range[" + std::to_string(x) + "," +
                                   std::to_string(y) + "]",
                               source + ": elevation outside declared range");
            }
            parsed.heights[idx] = value;
        }
    }
    *out = std::move(parsed);
    return Status::success();
}

Status loadHeightField(const std::string& path, HeightField* out) {
    std::vector<std::uint8_t> bytes;
    std::string error;
    if (!readFile(path, &bytes, &error)) {
        return Status::error(E_IO, "path", error);
    }
    return parseHeightField(bytes, path, out);
}

Status saveHeightField(const std::string& path, const HeightField& field) {
    return writeBinaryFile(path, encodeHeightField(field));
}

std::vector<std::uint8_t> encodeMaterialImage(const MaterialImage& material) {
    ByteWriter writer;
    writer.bytes("TMI1", 4);
    writer.u32(kMaterialHeaderSize);
    writer.u32(1);
    writer.u32(material.width);
    writer.u32(material.height);
    writer.u32(material.channels);
    writer.f32(material.cell_spacing);
    writer.u32(material.palette_size);
    writer.u64(material.baseline_version);
    const std::uint64_t payload_bytes =
        static_cast<std::uint64_t>(material.indices.size()) * 4ull;
    writer.u64(payload_bytes);
    const auto digest = hashU32(material.indices);
    writer.bytes(digest.data(), digest.size());
    for (std::uint32_t index : material.indices) {
        writer.u32(index);
    }
    return writer.data();
}

Status parseMaterialImage(const std::vector<std::uint8_t>& bytes,
                          const std::string& source, MaterialImage* out) {
    ByteReader reader(bytes.data(), bytes.size());
    std::string field;
    std::string message;
    if (!readHeaderPrefix(&reader, "TMI1", kMaterialHeaderSize, source, &field,
                          &message)) {
        return invalid(field, message);
    }
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::uint32_t channels = 0;
    float spacing = 0.0f;
    std::uint32_t palette_size = 0;
    std::uint64_t baseline_version = 0;
    std::uint64_t payload_bytes = 0;
    Digest payload_hash{};
    if (!reader.u32(&width) || !reader.u32(&height) ||
        !reader.u32(&channels) || !reader.f32(&spacing) ||
        !reader.u32(&palette_size) || !reader.u64(&baseline_version) ||
        !reader.u64(&payload_bytes) ||
        !reader.bytes(payload_hash.data(), payload_hash.size())) {
        return invalid("material.header",
                       source + ": truncated material header");
    }
    if (width < 2) {
        return invalid("material.width",
                       source + ": material width must be >= 2");
    }
    if (height < 2) {
        return invalid("material.height",
                       source + ": material height must be >= 2");
    }
    if (width > kMaxDimension || height > kMaxDimension) {
        return invalid("material.width",
                       source + ": dimensions exceed 65535 block-id limit");
    }
    if (static_cast<std::uint64_t>(width) * height > kMaxCells) {
        return invalid("material.width",
                       source + ": dimensions exceed safety limit");
    }
    if (channels != 1) {
        return invalid("material.channels",
                       source + ": only single-channel indices are supported");
    }
    if (!finiteFloat(spacing) || spacing <= 0.0f) {
        return invalid("material.cell_spacing",
                       source + ": cell spacing must be finite and positive");
    }
    if (palette_size == 0 || palette_size > 256) {
        return invalid("material.palette_size",
                       source + ": palette size must be in [1,256]");
    }
    const std::uint64_t expected_bytes =
        static_cast<std::uint64_t>(width) * height * 4ull;
    if (payload_bytes != expected_bytes || reader.remaining() != expected_bytes) {
        return invalid("material.payload_bytes",
                       source + ": payload length does not match header");
    }
    const Digest actual_hash = sha256(reader.current(), reader.remaining());
    if (actual_hash != payload_hash) {
        return invalid("material.payload_hash",
                       source + ": declared content hash does not match data");
    }
    MaterialImage parsed;
    parsed.width = width;
    parsed.height = height;
    parsed.cell_spacing = spacing;
    parsed.channels = channels;
    parsed.palette_size = palette_size;
    parsed.baseline_version = baseline_version;
    parsed.payload_hash = toHex(payload_hash);
    parsed.indices.resize(static_cast<std::size_t>(width) * height);
    ByteReader payload(reader.current(), reader.remaining());
    for (std::size_t i = 0; i < parsed.indices.size(); ++i) {
        std::uint32_t index = 0;
        if (!payload.u32(&index)) {
            return invalid("material.payload_bytes",
                           source + ": truncated material payload");
        }
        if (index >= palette_size) {
            return invalid("material.index[" + std::to_string(i) + "]",
                           source + ": material index outside palette");
        }
        parsed.indices[i] = index;
    }
    *out = std::move(parsed);
    return Status::success();
}

Status loadMaterialImage(const std::string& path, MaterialImage* out) {
    std::vector<std::uint8_t> bytes;
    std::string error;
    if (!readFile(path, &bytes, &error)) {
        return Status::error(E_IO, "path", error);
    }
    return parseMaterialImage(bytes, path, out);
}

Status saveMaterialImage(const std::string& path,
                         const MaterialImage& material) {
    return writeBinaryFile(path, encodeMaterialImage(material));
}

namespace {

template <typename PatchT, bool IsMaterial>
Status parsePatch(const std::vector<std::uint8_t>& bytes,
                  const std::string& source, const char* magic,
                  PatchT* out) {
    ByteReader reader(bytes.data(), bytes.size());
    std::string field;
    std::string message;
    if (!readHeaderPrefix(&reader, magic, kPatchHeaderSize, source, &field,
                          &message)) {
        return invalid(field, message);
    }
    std::uint32_t target_x = 0;
    std::uint32_t target_y = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    float spacing = 0.0f;
    std::uint32_t type_or_channels = 0;
    std::uint32_t reserved = 0;
    std::uint64_t baseline = 0;
    std::uint64_t sequence = 0;
    std::uint64_t payload_bytes = 0;
    Digest payload_hash{};
    if (!reader.u32(&target_x) || !reader.u32(&target_y) ||
        !reader.u32(&width) || !reader.u32(&height) ||
        !reader.f32(&spacing) || !reader.u32(&type_or_channels) ||
        !reader.u32(&reserved) || !reader.u64(&baseline) ||
        !reader.u64(&sequence) || !reader.u64(&payload_bytes) ||
        !reader.bytes(payload_hash.data(), payload_hash.size())) {
        return invalid("patch.header", source + ": truncated patch header");
    }
    if (reserved != 0) {
        return invalid("patch.reserved",
                       source + ": reserved field must be zero");
    }
    if (width == 0 || height == 0) {
        return invalid("patch.width",
                       source + ": patch dimensions must be positive");
    }
    if (static_cast<std::uint64_t>(width) * height > kMaxCells) {
        return invalid("patch.width",
                       source + ": dimensions exceed safety limit");
    }
    if (!finiteFloat(spacing) || spacing <= 0.0f) {
        return invalid("patch.cell_spacing",
                       source + ": cell spacing must be finite and positive");
    }
    if (!IsMaterial && type_or_channels != 0) {
        return invalid("patch.height_type",
                       source + ": only float32 (0) is supported");
    }
    if (IsMaterial && type_or_channels != 1) {
        return invalid("patch.channels",
                       source + ": material patches require one channel");
    }
    const std::uint64_t expected_bytes =
        static_cast<std::uint64_t>(width) * height * 4ull;
    if (payload_bytes != expected_bytes || reader.remaining() != expected_bytes) {
        return invalid("patch.payload_bytes",
                       source + ": payload length does not match header");
    }
    const Digest actual_hash = sha256(reader.current(), reader.remaining());
    if (actual_hash != payload_hash) {
        return invalid("patch.payload_hash",
                       source + ": declared content hash does not match data");
    }
    PatchT parsed;
    parsed.target = Rect{target_x, target_y, width, height};
    parsed.cell_spacing = spacing;
    if constexpr (IsMaterial) {
        parsed.channels = type_or_channels;
    } else {
        parsed.height_type = type_or_channels;
    }
    parsed.baseline_version = baseline;
    parsed.sequence = sequence;
    parsed.payload_hash = toHex(payload_hash);
    parsed.source_path = source;
    ByteReader payload(reader.current(), reader.remaining());
    if constexpr (IsMaterial) {
        parsed.indices.resize(static_cast<std::size_t>(width) * height);
        for (std::size_t i = 0; i < parsed.indices.size(); ++i) {
            if (!payload.u32(&parsed.indices[i])) {
                return invalid("patch.payload_bytes",
                               source + ": truncated material patch");
            }
        }
    } else {
        parsed.values.resize(static_cast<std::size_t>(width) * height);
        for (std::size_t i = 0; i < parsed.values.size(); ++i) {
            float value = 0.0f;
            if (!payload.f32(&value)) {
                return invalid("patch.payload_bytes",
                               source + ": truncated height patch");
            }
            if (!std::isfinite(value)) {
                return invalid("patch.value[" + std::to_string(i) + "]",
                               source + ": patch elevation is not finite");
            }
            parsed.values[i] = value;
        }
    }
    *out = std::move(parsed);
    return Status::success();
}

template <typename PatchT, bool IsMaterial>
std::vector<std::uint8_t> encodePatch(const PatchT& patch, const char* magic) {
    ByteWriter writer;
    writer.bytes(magic, 4);
    writer.u32(kPatchHeaderSize);
    writer.u32(1);
    writer.u32(patch.target.x);
    writer.u32(patch.target.y);
    writer.u32(patch.target.width);
    writer.u32(patch.target.height);
    writer.f32(patch.cell_spacing);
    if constexpr (IsMaterial) {
        writer.u32(patch.channels);
    } else {
        writer.u32(patch.height_type);
    }
    writer.u32(0);
    writer.u64(patch.baseline_version);
    writer.u64(patch.sequence);
    const std::uint64_t payload_bytes =
        static_cast<std::uint64_t>(patch.target.width) * patch.target.height *
        4ull;
    writer.u64(payload_bytes);
    Digest digest{};
    if constexpr (IsMaterial) {
        digest = hashU32(patch.indices);
    } else {
        digest = hashFloats(patch.values);
    }
    writer.bytes(digest.data(), digest.size());
    if constexpr (IsMaterial) {
        for (std::uint32_t value : patch.indices) writer.u32(value);
    } else {
        for (float value : patch.values) writer.f32(value);
    }
    return writer.data();
}

}  // namespace

Status parseHeightPatch(const std::vector<std::uint8_t>& bytes,
                        const std::string& source, HeightPatch* out) {
    return parsePatch<HeightPatch, false>(bytes, source, "THP1", out);
}

Status loadHeightPatch(const std::string& path, HeightPatch* out) {
    std::vector<std::uint8_t> bytes;
    std::string error;
    if (!readFile(path, &bytes, &error)) {
        return Status::error(E_IO, "path", error);
    }
    return parseHeightPatch(bytes, path, out);
}

std::vector<std::uint8_t> encodeHeightPatch(const HeightPatch& patch) {
    return encodePatch<HeightPatch, false>(patch, "THP1");
}

Status saveHeightPatch(const std::string& path, const HeightPatch& patch) {
    return writeBinaryFile(path, encodeHeightPatch(patch));
}

Status parseMaterialPatch(const std::vector<std::uint8_t>& bytes,
                          const std::string& source, MaterialPatch* out) {
    return parsePatch<MaterialPatch, true>(bytes, source, "TMP1", out);
}

Status loadMaterialPatch(const std::string& path, MaterialPatch* out) {
    std::vector<std::uint8_t> bytes;
    std::string error;
    if (!readFile(path, &bytes, &error)) {
        return Status::error(E_IO, "path", error);
    }
    return parseMaterialPatch(bytes, path, out);
}

std::vector<std::uint8_t> encodeMaterialPatch(const MaterialPatch& patch) {
    return encodePatch<MaterialPatch, true>(patch, "TMP1");
}

Status saveMaterialPatch(const std::string& path,
                         const MaterialPatch& patch) {
    return writeBinaryFile(path, encodeMaterialPatch(patch));
}

Status validateMaterialDimensions(const HeightField& height,
                                  const MaterialImage& material) {
    if (material.width != height.width) {
        return invalid("material.width",
                       "material width does not match height field");
    }
    if (material.height != height.height) {
        return invalid("material.height",
                       "material height does not match height field");
    }
    const double diff = std::fabs(static_cast<double>(material.cell_spacing) -
                                  static_cast<double>(height.cell_spacing));
    const double scale = std::max(1.0, std::fabs(
                                            static_cast<double>(
                                                height.cell_spacing)));
    if (diff > 1e-6 * scale) {
        return invalid("material.cell_spacing",
                       "material spacing does not match height field");
    }
    if (material.baseline_version != height.baseline_version) {
        return invalid("material.baseline_version",
                       "material baseline version is out of sync");
    }
    return Status::success();
}

Status validateBatch(const PatchBatch& batch, const HeightField& current,
                     const MaterialImage& material) {
    if (!batch.height && !batch.material) {
        return Status::error(E_INVALID_ARGUMENT, "batch",
                             "batch contains no patch");
    }
    if (batch.sequence == 0) {
        return Status::error(E_PATCH_STALE, "patch_sequence",
                             "patch sequence must be positive");
    }
    if (batch.height) {
        const HeightPatch& patch = *batch.height;
        if (patch.target.empty()) {
            return Status::error(E_PATCH_BOUNDS, "target",
                                 "height patch rectangle is empty");
        }
        if (patch.target.right() > current.width ||
            patch.target.bottom() > current.height) {
            return Status::error(E_PATCH_BOUNDS, "target",
                                 "height patch rectangle exceeds terrain");
        }
        const double spacing_diff =
            std::fabs(static_cast<double>(patch.cell_spacing) -
                      static_cast<double>(current.cell_spacing));
        if (spacing_diff >
            1e-6 * std::max(1.0, static_cast<double>(current.cell_spacing))) {
            return Status::error(E_PATCH_BOUNDS, "cell_spacing",
                                 "height patch spacing does not match terrain");
        }
        if (patch.height_type != current.height_type) {
            return Status::error(E_PATCH_BOUNDS, "height_type",
                                 "height patch type does not match terrain");
        }
        if (patch.values.size() !=
            static_cast<std::size_t>(patch.target.width) *
                patch.target.height) {
            return Status::error(E_PATCH_BOUNDS, "payload_bytes",
                                 "height patch payload length mismatch");
        }
        for (std::size_t i = 0; i < patch.values.size(); ++i) {
            const float value = patch.values[i];
            if (!std::isfinite(value)) {
                return Status::error(E_HEIGHT_INVALID, "patch.value",
                                     "height patch contains non-finite value");
            }
            if (value < current.valid_min || value > current.valid_max) {
                return Status::error(
                    E_HEIGHT_INVALID,
                    "patch.valid_range[" + std::to_string(i) + "]",
                    "height patch value is outside terrain valid range");
            }
        }
    }
    if (batch.material) {
        const MaterialPatch& patch = *batch.material;
        if (patch.target.empty()) {
            return Status::error(E_PATCH_BOUNDS, "target",
                                 "material patch rectangle is empty");
        }
        if (patch.target.right() > material.width ||
            patch.target.bottom() > material.height) {
            return Status::error(E_PATCH_BOUNDS, "target",
                                 "material patch rectangle exceeds image");
        }
        const double spacing_diff =
            std::fabs(static_cast<double>(patch.cell_spacing) -
                      static_cast<double>(material.cell_spacing));
        if (spacing_diff >
            1e-6 * std::max(1.0, static_cast<double>(material.cell_spacing))) {
            return Status::error(E_PATCH_BOUNDS, "cell_spacing",
                                 "material patch spacing does not match image");
        }
        if (patch.channels != material.channels) {
            return Status::error(E_PATCH_BOUNDS, "channels",
                                 "material patch channel count mismatch");
        }
        if (patch.indices.size() !=
            static_cast<std::size_t>(patch.target.width) *
                patch.target.height) {
            return Status::error(E_PATCH_BOUNDS, "payload_bytes",
                                 "material patch payload length mismatch");
        }
        for (std::uint32_t index : patch.indices) {
            if (index >= material.palette_size) {
                return Status::error(E_HEIGHT_INVALID, "patch.material_index",
                                     "material patch index outside palette");
            }
        }
    }
    if (batch.height && batch.material) {
        if (batch.height->sequence != batch.material->sequence) {
            return Status::error(E_PATCH_STALE, "patch_sequence",
                                 "height and material patch sequences differ");
        }
        if (batch.height->baseline_version !=
            batch.material->baseline_version) {
            return Status::error(E_PATCH_STALE, "baseline_version",
                                 "height and material patch baselines differ");
        }
    }
    const std::uint64_t baseline =
        batch.height ? batch.height->baseline_version
                     : batch.material->baseline_version;
    const std::uint64_t sequence =
        batch.height ? batch.height->sequence : batch.material->sequence;
    if (baseline != current.baseline_version ||
        baseline != material.baseline_version ||
        sequence != current.baseline_version + 1 ||
        batch.sequence != sequence) {
        return Status::error(
            E_PATCH_STALE, "baseline_version",
            "patch baseline/sequence does not follow committed version " +
                std::to_string(current.baseline_version));
    }
    return Status::success();
}

}  // namespace terrain

