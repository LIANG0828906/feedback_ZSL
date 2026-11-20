#include "battle/trace.h"

#include "battle/bits.h"
#include "battle/sha256.h"

#include <charconv>
#include <algorithm>
#include <cctype>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <map>
#include <sstream>

namespace battle {
namespace {

constexpr const char* kZeroHash =
    "0000000000000000000000000000000000000000000000000000000000000000";

Error trace_error(std::string message, std::string point) {
    Error error;
    error.code = StatusCode::TraceInvalid;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

std::string join_bits(const float* values, std::size_t count) {
    std::string result;
    for (std::size_t i = 0; i < count; ++i) {
        if (i != 0) {
            result.push_back(',');
        }
        result += hex32(float_bits(values[i]));
    }
    return result;
}

std::string camera_field(const Camera& camera) {
    const float values[15] = {
        camera.position.x, camera.position.y, camera.position.z,
        camera.forward.x, camera.forward.y, camera.forward.z,
        camera.up.x, camera.up.y, camera.up.z,
        camera.fov_y_radians, camera.aspect,
        camera.viewport_width, camera.viewport_height,
        camera.near_plane, camera.far_plane};
    return join_bits(values, 15);
}

std::string ray_field(const Ray& ray) {
    const float values[6] = {
        ray.origin.x, ray.origin.y, ray.origin.z,
        ray.direction.x, ray.direction.y, ray.direction.z};
    return join_bits(values, 6);
}

std::string point_field(const glm::vec3& point) {
    const float values[3] = {point.x, point.y, point.z};
    return join_bits(values, 3);
}

std::string conflicts_field(const std::vector<EntityRef>& conflicts) {
    if (conflicts.empty()) {
        return "-";
    }
    std::string result;
    for (std::size_t i = 0; i < conflicts.size(); ++i) {
        if (i != 0) {
            result.push_back(',');
        }
        result += entity_ref_string(conflicts[i]);
    }
    return result;
}

std::string units_field(const std::vector<std::uint32_t>& units) {
    if (units.empty()) {
        return "-";
    }
    std::string result;
    for (std::size_t i = 0; i < units.size(); ++i) {
        if (i != 0) {
            result.push_back(',');
        }
        result += std::to_string(units[i]);
    }
    return result;
}

std::string vertices_field(const DraftRegion& region) {
    std::string result = std::to_string(region.vertices.size());
    for (const glm::vec3& vertex : region.vertices) {
        result.push_back(',');
        result += hex32(float_bits(vertex.x));
        result.push_back(',');
        result += hex32(float_bits(vertex.y));
        result.push_back(',');
        result += hex32(float_bits(vertex.z));
    }
    return result;
}

std::string finish_line(std::string body) {
    return body + "|record_hash=" + digest_hex(sha256(body));
}

std::vector<std::string> split(const std::string& text, char delimiter) {
    std::vector<std::string> parts;
    std::size_t start = 0;
    while (true) {
        const std::size_t position = text.find(delimiter, start);
        if (position == std::string::npos) {
            parts.push_back(text.substr(start));
            break;
        }
        parts.push_back(text.substr(start, position - start));
        start = position + 1;
    }
    return parts;
}

bool parse_u64(const std::string& text, std::uint64_t& value) {
    if (text.empty()) {
        return false;
    }
    const char* begin = text.data();
    const char* end = text.data() + text.size();
    const auto result = std::from_chars(begin, end, value, 10);
    return result.ec == std::errc() && result.ptr == end;
}

bool parse_i64(const std::string& text, std::int64_t& value) {
    if (text.empty()) {
        return false;
    }
    const char* begin = text.data();
    const char* end = text.data() + text.size();
    const auto result = std::from_chars(begin, end, value, 10);
    return result.ec == std::errc() && result.ptr == end;
}

bool parse_u32_decimal(const std::string& text, std::uint32_t& value) {
    std::uint64_t parsed = 0;
    if (!parse_u64(text, parsed) || parsed > 0xffffffffULL) {
        return false;
    }
    value = static_cast<std::uint32_t>(parsed);
    return true;
}

bool parse_bits(const std::string& text, std::vector<float>& values,
                std::size_t expected) {
    const std::vector<std::string> tokens = split(text, ',');
    if (tokens.size() != expected) {
        return false;
    }
    values.clear();
    values.reserve(expected);
    for (const std::string& token : tokens) {
        std::uint32_t bits = 0;
        if (!parse_hex32(token, bits)) {
            return false;
        }
        values.push_back(bits_float(bits));
    }
    return true;
}

bool parse_camera(const std::string& text, Camera& camera) {
    std::vector<float> values;
    if (!parse_bits(text, values, 15)) {
        return false;
    }
    camera.position = {values[0], values[1], values[2]};
    camera.forward = {values[3], values[4], values[5]};
    camera.up = {values[6], values[7], values[8]};
    camera.fov_y_radians = values[9];
    camera.aspect = values[10];
    camera.viewport_width = values[11];
    camera.viewport_height = values[12];
    camera.near_plane = values[13];
    camera.far_plane = values[14];
    return true;
}

bool parse_ray(const std::string& text, Ray& ray) {
    std::vector<float> values;
    if (!parse_bits(text, values, 6)) {
        return false;
    }
    ray.origin = {values[0], values[1], values[2]};
    ray.direction = {values[3], values[4], values[5]};
    return true;
}

bool parse_vec3(const std::string& text, glm::vec3& point) {
    std::vector<float> values;
    if (!parse_bits(text, values, 3)) {
        return false;
    }
    point = {values[0], values[1], values[2]};
    return true;
}

bool parse_screen(const std::string& text, ScreenPoint& screen) {
    std::vector<float> values;
    if (!parse_bits(text, values, 2)) {
        return false;
    }
    screen = {values[0], values[1]};
    return true;
}

bool parse_hit(const std::string& text, bool& has_hit, EntityRef& entity) {
    if (text == "none") {
        has_hit = false;
        entity = {};
        return true;
    }
    const std::vector<std::string> parts = split(text, ':');
    if (parts.size() != 2) {
        return false;
    }
    std::uint32_t id = 0;
    if (!parse_u32_decimal(parts[1], id)) {
        return false;
    }
    if (parts[0] == "unit") {
        entity = {EntityKind::Unit, id};
    } else if (parts[0] == "cover") {
        entity = {EntityKind::Cover, id};
    } else if (parts[0] == "ground") {
        entity = {EntityKind::Ground, id};
    } else {
        return false;
    }
    has_hit = true;
    return true;
}

bool parse_conflicts(const std::string& text,
                     std::vector<EntityRef>& conflicts) {
    conflicts.clear();
    if (text == "-" || text.empty()) {
        return true;
    }
    for (const std::string& token : split(text, ',')) {
        bool has_hit = false;
        EntityRef entity;
        if (!parse_hit(token, has_hit, entity) || !has_hit ||
            entity.kind == EntityKind::Ground) {
            return false;
        }
        conflicts.push_back(entity);
    }
    std::sort(conflicts.begin(), conflicts.end());
    return true;
}

bool parse_units(const std::string& text, std::vector<std::uint32_t>& units) {
    units.clear();
    if (text == "-" || text.empty()) {
        return true;
    }
    for (const std::string& token : split(text, ',')) {
        std::uint32_t id = 0;
        if (!parse_u32_decimal(token, id)) {
            return false;
        }
        units.push_back(id);
    }
    return true;
}

bool parse_vertices(const std::string& text, DraftRegion& region) {
    const std::vector<std::string> parts = split(text, ',');
    if (parts.empty()) {
        return false;
    }
    std::uint32_t count = 0;
    if (!parse_u32_decimal(parts[0], count) || parts.size() != 1U + count * 3U) {
        return false;
    }
    region.vertices.clear();
    region.vertices.reserve(count);
    for (std::uint32_t i = 0; i < count; ++i) {
        std::uint32_t x = 0;
        std::uint32_t y = 0;
        std::uint32_t z = 0;
        if (!parse_hex32(parts[1 + i * 3], x) ||
            !parse_hex32(parts[2 + i * 3], y) ||
            !parse_hex32(parts[3 + i * 3], z)) {
            return false;
        }
        region.vertices.push_back({bits_float(x), bits_float(y), bits_float(z)});
    }
    return true;
}

bool parse_cell(const std::string& text, std::uint32_t& x, std::uint32_t& z) {
    const std::vector<std::string> parts = split(text, ':');
    return parts.size() == 2 && parse_u32_decimal(parts[0], x) &&
           parse_u32_decimal(parts[1], z);
}

bool parse_reason(const std::string& text, PlacementReason& reason) {
    if (text == "LEGAL") {
        reason = PlacementReason::Legal;
    } else if (text == "BLOCKED_CELL") {
        reason = PlacementReason::BlockedCell;
    } else if (text == "UNIT_OVERLAP") {
        reason = PlacementReason::UnitOverlap;
    } else if (text == "COVER_OVERLAP") {
        reason = PlacementReason::CoverOverlap;
    } else {
        return false;
    }
    return true;
}

bool parse_bool01(const std::string& text, bool& value) {
    if (text == "1") {
        value = true;
        return true;
    }
    if (text == "0") {
        value = false;
        return true;
    }
    return false;
}

struct ParsedFields {
    std::string type;
    std::map<std::string, std::string> values;
    std::string body;
};

Outcome<ParsedFields> parse_fields(const std::string& line,
                                   std::uint64_t line_number) {
    const std::string point_prefix =
        "trace.line." + std::to_string(line_number) + ".";
    ParsedFields parsed;
    const std::vector<std::string> parts = split(line, '|');
    if (parts.empty() || parts[0].empty()) {
        return Outcome<ParsedFields>::failure(trace_error(
            "trace line has no record type", point_prefix + "type"));
    }
    parsed.type = parts[0];
    for (std::size_t i = 1; i < parts.size(); ++i) {
        const std::size_t equal = parts[i].find('=');
        if (equal == std::string::npos || equal == 0) {
            return Outcome<ParsedFields>::failure(trace_error(
                "trace field is not key=value", point_prefix + "field"));
        }
        const std::string key = parts[i].substr(0, equal);
        const std::string value = parts[i].substr(equal + 1);
        std::string lower = key;
        std::transform(lower.begin(), lower.end(), lower.begin(),
                       [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
        if (lower.find("time") != std::string::npos) {
            return Outcome<ParsedFields>::failure(trace_error(
                "timestamp-derived trace fields are forbidden",
                point_prefix + key));
        }
        if (!parsed.values.emplace(key, value).second) {
            return Outcome<ParsedFields>::failure(trace_error(
                "duplicate trace field", point_prefix + key));
        }
    }
    const std::string marker = "record_hash=";
    const std::size_t marker_pos = line.rfind(marker);
    if (marker_pos == std::string::npos ||
        marker_pos < 2 || line[marker_pos - 1] != '|') {
        return Outcome<ParsedFields>::failure(trace_error(
            "trace record hash is missing", point_prefix + "record_hash"));
    }
    if (marker_pos + marker.size() + 64U != line.size()) {
        return Outcome<ParsedFields>::failure(trace_error(
            "record hash must be the final 64-hex field",
            point_prefix + "record_hash"));
    }
    parsed.body = line.substr(0, marker_pos - 1);
    return Outcome<ParsedFields>::success(std::move(parsed));
}

Outcome<TraceRecord> parse_record(const std::string& line,
                                  std::uint64_t line_number) {
    Outcome<ParsedFields> parsed_outcome = parse_fields(line, line_number);
    if (!parsed_outcome) {
        return Outcome<TraceRecord>::failure(parsed_outcome.error());
    }
    const ParsedFields& parsed = parsed_outcome.value();
    const std::string prefix = "trace.line." + std::to_string(line_number) + ".";
    auto required = [&](const std::string& key) -> const std::string* {
        const auto it = parsed.values.find(key);
        if (it == parsed.values.end()) {
            return nullptr;
        }
        return &it->second;
    };
    auto missing = [&](const std::string& key) {
        return Outcome<TraceRecord>::failure(trace_error(
            "required trace field is missing: " + key, prefix + key));
    };

    TraceRecord record;
    if (parsed.type == "PICK") {
        record.type = TraceType::Pick;
    } else if (parsed.type == "BOX") {
        record.type = TraceType::Box;
    } else if (parsed.type == "DRAFT") {
        record.type = TraceType::Draft;
    } else {
        return Outcome<TraceRecord>::failure(trace_error(
            "unknown trace record type", prefix + "type"));
    }

    const std::string* seq = required("seq");
    const std::string* version = required("version");
    const std::string* region_hash = required("region_hash");
    const std::string* previous_hash = required("prev_hash");
    const std::string* record_hash = required("record_hash");
    const std::string* status = required("status");
    if (seq == nullptr) return missing("seq");
    if (version == nullptr) return missing("version");
    if (region_hash == nullptr) return missing("region_hash");
    if (previous_hash == nullptr) return missing("prev_hash");
    if (record_hash == nullptr) return missing("record_hash");
    if (status == nullptr) return missing("status");
    if (!parse_u64(*seq, record.sequence)) {
        return Outcome<TraceRecord>::failure(trace_error(
            "invalid trace sequence", prefix + "seq"));
    }
    if (!parse_u64(*version, record.version) || record.version == 0) {
        return Outcome<TraceRecord>::failure(trace_error(
            "trace region version is missing or zero", prefix + "version"));
    }
    Digest parsed_digest{};
    if (!parse_digest_hex(*region_hash, parsed_digest)) {
        return Outcome<TraceRecord>::failure(trace_error(
            "invalid region hash", prefix + "region_hash"));
    }
    if (!parse_digest_hex(*previous_hash, parsed_digest)) {
        return Outcome<TraceRecord>::failure(trace_error(
            "invalid previous hash", prefix + "prev_hash"));
    }
    if (!parse_digest_hex(*record_hash, parsed_digest)) {
        return Outcome<TraceRecord>::failure(trace_error(
            "invalid record hash", prefix + "record_hash"));
    }
    std::uint32_t status_number = 0;
    if (!parse_u32_decimal(*status, status_number)) {
        return Outcome<TraceRecord>::failure(trace_error(
            "invalid status code", prefix + "status"));
    }
    record.status = static_cast<StatusCode>(status_number);
    record.region_hash = *region_hash;
    record.previous_hash = *previous_hash;
    record.record_hash = *record_hash;
    record.error_name = required("error") != nullptr ? *required("error") : "";
    record.injection_point =
        required("injection") != nullptr ? *required("injection") : "-";

    if (record.type == TraceType::Pick) {
        const std::string* screen = required("screen");
        const std::string* camera = required("camera");
        const std::string* ray = required("ray");
        const std::string* hit = required("hit");
        const std::string* point = required("point");
        const std::string* normal = required("normal");
        const std::string* distance = required("distance");
        if (screen == nullptr) return missing("screen");
        if (camera == nullptr) return missing("camera");
        if (ray == nullptr) return missing("ray");
        if (hit == nullptr) return missing("hit");
        if (point == nullptr) return missing("point");
        if (normal == nullptr) return missing("normal");
        if (distance == nullptr) return missing("distance");
        if (!parse_screen(*screen, record.pick.screen) ||
            !parse_camera(*camera, record.pick.camera) ||
            !parse_ray(*ray, record.recorded_ray) ||
            !parse_hit(*hit, record.has_hit, record.hit.entity) ||
            !parse_vec3(*point, record.hit.point) ||
            !parse_vec3(*normal, record.hit.normal)) {
            return Outcome<TraceRecord>::failure(trace_error(
                "invalid PICK trace payload", prefix + "payload"));
        }
        std::uint32_t distance_bits = 0;
        if (!parse_hex32(*distance, distance_bits)) {
            return Outcome<TraceRecord>::failure(trace_error(
                "invalid hit distance", prefix + "distance"));
        }
        record.hit.distance = bits_float(distance_bits);
        record.pick.expected_version = record.version;
        record.pick.expected_hash = record.region_hash;
    } else if (record.type == TraceType::Box) {
        const std::string* rect = required("rect");
        const std::string* camera = required("camera");
        const std::string* units = required("units");
        if (rect == nullptr) return missing("rect");
        if (camera == nullptr) return missing("camera");
        if (units == nullptr) return missing("units");
        std::vector<float> rect_values;
        if (!parse_bits(*rect, rect_values, 4) ||
            !parse_camera(*camera, record.box.camera) ||
            !parse_units(*units, record.box_units)) {
            return Outcome<TraceRecord>::failure(trace_error(
                "invalid BOX trace payload", prefix + "payload"));
        }
        record.box.rect = {
            rect_values[0], rect_values[1], rect_values[2], rect_values[3]};
        record.box.expected_version = record.version;
        record.box.expected_hash = record.region_hash;
    } else {
        const std::string* vertices = required("vertices");
        const std::string* legal = required("legal");
        const std::string* reason = required("reason");
        const std::string* failed_vertex = required("failed_vertex");
        const std::string* failed_cell = required("failed_cell");
        const std::string* conflicts = required("conflicts");
        if (vertices == nullptr) return missing("vertices");
        if (legal == nullptr) return missing("legal");
        if (reason == nullptr) return missing("reason");
        if (failed_vertex == nullptr) return missing("failed_vertex");
        if (failed_cell == nullptr) return missing("failed_cell");
        if (conflicts == nullptr) return missing("conflicts");
        std::int64_t failed = -1;
        if (!parse_vertices(*vertices, record.draft.region) ||
            !parse_bool01(*legal, record.draft_legal) ||
            !parse_reason(*reason, record.draft_reason) ||
            !parse_i64(*failed_vertex, failed) ||
            !parse_cell(*failed_cell, record.failed_cell_x,
                        record.failed_cell_z) ||
            !parse_conflicts(*conflicts, record.conflicts)) {
            return Outcome<TraceRecord>::failure(trace_error(
                "invalid DRAFT trace payload", prefix + "payload"));
        }
        record.failed_vertex = static_cast<int>(failed);
        record.draft.expected_version = record.version;
        record.draft.expected_hash = record.region_hash;
    }
    return Outcome<TraceRecord>::success(std::move(record));
}

}  // namespace

void TraceRecorder::record_pick(std::uint64_t version,
                                const std::string& region_hash,
                                const PickRequest& request, const Ray& ray,
                                const Outcome<PickResult>& result) {
    StatusCode status = StatusCode::Ok;
    std::string error_name = "OK";
    std::string injection = "-";
    EntityRef entity;
    bool has_hit = false;
    glm::vec3 point(0.0F);
    glm::vec3 normal(0.0F);
    float distance = 0.0F;
    if (result) {
        has_hit = result.value().has_hit;
        if (has_hit) {
            entity = result.value().hit.entity;
            point = result.value().hit.point;
            normal = result.value().hit.normal;
            distance = result.value().hit.distance;
        }
    } else {
        status = result.error().code;
        error_name = status_name(status);
        injection = result.error().injection_point.empty()
                        ? "-"
                        : result.error().injection_point;
    }
    const float screen_values[2] = {request.screen.x, request.screen.y};
    std::string body = "PICK";
    body += "|seq=" + std::to_string(next_sequence_);
    body += "|version=" + std::to_string(version);
    body += "|region_hash=" + region_hash;
    body += "|prev_hash=" + previous_hash_;
    body += "|screen=" + join_bits(screen_values, 2);
    body += "|camera=" + camera_field(request.camera);
    body += "|ray=" + ray_field(ray);
    body += "|hit=" + (has_hit ? entity_ref_string(entity) : "none");
    body += "|point=" + point_field(point);
    body += "|normal=" + point_field(normal);
    body += "|distance=" + hex32(float_bits(distance));
    body += "|status=" + std::to_string(static_cast<std::uint32_t>(status));
    body += "|error=" + error_name;
    body += "|injection=" + injection;
    const std::string line = finish_line(std::move(body));
    previous_hash_ = line.substr(line.rfind("record_hash=") + 12);
    ++next_sequence_;
    lines_.push_back(line);
}

void TraceRecorder::record_box(std::uint64_t version,
                               const std::string& region_hash,
                               const BoxSelectRequest& request,
                               const Outcome<BoxSelectResult>& result) {
    StatusCode status = StatusCode::Ok;
    std::string error_name = "OK";
    std::string injection = "-";
    std::vector<std::uint32_t> units;
    if (result) {
        units = result.value().unit_ids;
    } else {
        status = result.error().code;
        error_name = status_name(status);
        injection = result.error().injection_point.empty()
                        ? "-"
                        : result.error().injection_point;
    }
    const float rect_values[4] = {
        request.rect.min_x, request.rect.min_y,
        request.rect.max_x, request.rect.max_y};
    std::string body = "BOX";
    body += "|seq=" + std::to_string(next_sequence_);
    body += "|version=" + std::to_string(version);
    body += "|region_hash=" + region_hash;
    body += "|prev_hash=" + previous_hash_;
    body += "|rect=" + join_bits(rect_values, 4);
    body += "|camera=" + camera_field(request.camera);
    body += "|units=" + units_field(units);
    body += "|status=" + std::to_string(static_cast<std::uint32_t>(status));
    body += "|error=" + error_name;
    body += "|injection=" + injection;
    const std::string line = finish_line(std::move(body));
    previous_hash_ = line.substr(line.rfind("record_hash=") + 12);
    ++next_sequence_;
    lines_.push_back(line);
}

void TraceRecorder::record_placement(
    std::uint64_t version, const std::string& region_hash,
    const PlacementRequest& request,
    const Outcome<PlacementResult>& result) {
    StatusCode status = StatusCode::Ok;
    std::string error_name = "OK";
    std::string injection = "-";
    bool legal = false;
    PlacementReason reason = PlacementReason::Legal;
    int failed_vertex = -1;
    std::uint32_t failed_cell_x = 0;
    std::uint32_t failed_cell_z = 0;
    std::vector<EntityRef> conflicts;
    if (result) {
        legal = result.value().legal;
        reason = result.value().reason;
        failed_vertex = result.value().failed_vertex;
        failed_cell_x = result.value().failed_cell_x;
        failed_cell_z = result.value().failed_cell_z;
        conflicts = result.value().conflicts;
    } else {
        status = result.error().code;
        error_name = status_name(status);
        injection = result.error().injection_point.empty()
                        ? "-"
                        : result.error().injection_point;
        failed_vertex = result.error().vertex_index;
        conflicts = result.error().conflicts;
    }
    std::string body = "DRAFT";
    body += "|seq=" + std::to_string(next_sequence_);
    body += "|version=" + std::to_string(version);
    body += "|region_hash=" + region_hash;
    body += "|prev_hash=" + previous_hash_;
    body += "|vertices=" + vertices_field(request.region);
    body += "|legal=" + std::string(legal ? "1" : "0");
    body += "|reason=" + std::string(placement_reason_name(reason));
    body += "|failed_vertex=" + std::to_string(failed_vertex);
    body += "|failed_cell=" + std::to_string(failed_cell_x) + ":" +
            std::to_string(failed_cell_z);
    body += "|conflicts=" + conflicts_field(conflicts);
    body += "|status=" + std::to_string(static_cast<std::uint32_t>(status));
    body += "|error=" + error_name;
    body += "|injection=" + injection;
    const std::string line = finish_line(std::move(body));
    previous_hash_ = line.substr(line.rfind("record_hash=") + 12);
    ++next_sequence_;
    lines_.push_back(line);
}

std::string TraceRecorder::text() const {
    std::string result;
    for (const std::string& line : lines_) {
        result += line;
        result.push_back('\n');
    }
    return result;
}

Outcome<void> TraceRecorder::write_file(const std::string& path) const {
    const std::filesystem::path output_path(path);
    std::error_code error;
    if (output_path.has_parent_path()) {
        std::filesystem::create_directories(output_path.parent_path(), error);
        if (error) {
            return Outcome<void>::failure(trace_error(
                "cannot create trace directory: " +
                    output_path.parent_path().string(),
                "trace.file.directory"));
        }
    }
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        return Outcome<void>::failure(trace_error(
            "cannot open trace file for writing: " + path, "trace.file.open"));
    }
    const std::string content = text();
    stream.write(content.data(), static_cast<std::streamsize>(content.size()));
    if (!stream) {
        return Outcome<void>::failure(trace_error(
            "cannot write trace file: " + path, "trace.file.write"));
    }
    return Outcome<void>::success();
}

Outcome<TraceFile> load_trace(const std::string& path) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        return Outcome<TraceFile>::failure(trace_error(
            "cannot open trace file: " + path, "trace.file.open"));
    }
    TraceFile file;
    file.text.assign(std::istreambuf_iterator<char>(stream),
                     std::istreambuf_iterator<char>());
    std::istringstream lines(file.text);
    std::string line;
    std::uint64_t line_number = 0;
    std::string previous = kZeroHash;
    std::uint64_t expected_sequence = 0;
    while (std::getline(lines, line)) {
        ++line_number;
        if (!line.empty() && line.back() == '\r') {
            line.pop_back();
        }
        if (line.empty()) {
            continue;
        }
        Outcome<TraceRecord> parsed = parse_record(line, line_number);
        if (!parsed) {
            return Outcome<TraceFile>::failure(parsed.error());
        }
        TraceRecord& record = parsed.value();
        if (record.sequence != expected_sequence) {
            return Outcome<TraceFile>::failure(trace_error(
                "trace sequence is not contiguous",
                "trace.line." + std::to_string(line_number) + ".seq"));
        }
        if (record.previous_hash != previous) {
            return Outcome<TraceFile>::failure(trace_error(
                "trace hash chain is broken",
                "trace.line." + std::to_string(line_number) + ".prev_hash"));
        }
        const std::string computed = digest_hex(sha256(
            line.substr(0, line.rfind("|record_hash="))));
        if (computed != record.record_hash) {
            return Outcome<TraceFile>::failure(trace_error(
                "trace record hash mismatch",
                "trace.line." + std::to_string(line_number) + ".record_hash"));
        }
        previous = record.record_hash;
        ++expected_sequence;
        file.records.push_back(std::move(record));
    }
    file.final_hash = previous;
    return Outcome<TraceFile>::success(std::move(file));
}

Outcome<std::vector<RegionState>> load_snapshot_directory(
    const std::string& path) {
    namespace fs = std::filesystem;
    std::error_code error;
    if (!fs::is_directory(path, error)) {
        return Outcome<std::vector<RegionState>>::failure(trace_error(
            "snapshot path is not a directory: " + path,
            "snapshot.directory"));
    }
    std::vector<fs::path> files;
    for (const fs::directory_entry& entry : fs::directory_iterator(path, error)) {
        if (error) {
            return Outcome<std::vector<RegionState>>::failure(trace_error(
                "cannot enumerate snapshot directory", "snapshot.directory.iterate"));
        }
        if (entry.is_regular_file() && entry.path().extension() == ".region") {
            files.push_back(entry.path());
        }
    }
    std::sort(files.begin(), files.end());
    std::vector<RegionState> states;
    for (const fs::path& file : files) {
        Outcome<RegionState> state = load_region_snapshot(file.string());
        if (!state) {
            return Outcome<std::vector<RegionState>>::failure(state.error());
        }
        states.push_back(std::move(state.value()));
    }
    if (states.empty()) {
        return Outcome<std::vector<RegionState>>::failure(trace_error(
            "snapshot directory has no .region files", "snapshot.directory.empty"));
    }
    return Outcome<std::vector<RegionState>>::success(std::move(states));
}

Outcome<ReplayReport> replay_trace(
    const TraceFile& trace, const std::vector<RegionState>& snapshots) {
    Outcome<std::shared_ptr<RegionStore>> created =
        RegionStore::create_archive(snapshots);
    if (!created) {
        return Outcome<ReplayReport>::failure(created.error());
    }
    const std::shared_ptr<RegionStore>& store = created.value();
    ReplayReport report;
    for (const TraceRecord& record : trace.records) {
        Outcome<SnapshotHandle> acquired =
            store->acquire_version(record.version, record.region_hash);
        if (!acquired) {
            return Outcome<ReplayReport>::failure(trace_error(
                "trace references an unavailable region version: " +
                    std::to_string(record.version),
                "trace.replay." + std::to_string(record.sequence) + ".version"));
        }
        SnapshotHandle handle = acquired.value();
        bool matched = false;
        if (record.type == TraceType::Pick) {
            Outcome<PickResult> actual = pick_screen(*store, handle, record.pick);
            if (record.status == StatusCode::Ok) {
                matched = actual.ok() && actual.value().region_version == record.version &&
                          actual.value().region_hash == record.region_hash &&
                          actual.value().has_hit == record.has_hit;
                if (matched && record.has_hit) {
                    const Hit& lhs = actual.value().hit;
                    matched = lhs.entity == record.hit.entity &&
                              float_bits(lhs.point.x) == float_bits(record.hit.point.x) &&
                              float_bits(lhs.point.y) == float_bits(record.hit.point.y) &&
                              float_bits(lhs.point.z) == float_bits(record.hit.point.z) &&
                              float_bits(lhs.normal.x) == float_bits(record.hit.normal.x) &&
                              float_bits(lhs.normal.y) == float_bits(record.hit.normal.y) &&
                              float_bits(lhs.normal.z) == float_bits(record.hit.normal.z) &&
                              float_bits(lhs.distance) == float_bits(record.hit.distance);
                }
                if (matched) {
                    const Ray& ray = actual.value().ray;
                    matched = float_bits(ray.origin.x) ==
                                  float_bits(record.recorded_ray.origin.x) &&
                              float_bits(ray.origin.y) ==
                                  float_bits(record.recorded_ray.origin.y) &&
                              float_bits(ray.origin.z) ==
                                  float_bits(record.recorded_ray.origin.z) &&
                              float_bits(ray.direction.x) ==
                                  float_bits(record.recorded_ray.direction.x) &&
                              float_bits(ray.direction.y) ==
                                  float_bits(record.recorded_ray.direction.y) &&
                              float_bits(ray.direction.z) ==
                                  float_bits(record.recorded_ray.direction.z);
                }
            } else {
                matched = !actual.ok() && actual.error().code == record.status;
            }
        } else if (record.type == TraceType::Box) {
            Outcome<BoxSelectResult> actual = box_select(*store, handle, record.box);
            if (record.status == StatusCode::Ok) {
                matched = actual.ok() &&
                          actual.value().region_version == record.version &&
                          actual.value().region_hash == record.region_hash &&
                          actual.value().unit_ids == record.box_units;
            } else {
                matched = !actual.ok() && actual.error().code == record.status;
            }
        } else {
            Outcome<PlacementResult> actual =
                validate_placement(*store, handle, record.draft);
            if (record.status == StatusCode::Ok) {
                matched = actual.ok() &&
                          actual.value().region_version == record.version &&
                          actual.value().region_hash == record.region_hash &&
                          actual.value().legal == record.draft_legal &&
                          actual.value().reason == record.draft_reason &&
                          actual.value().failed_vertex == record.failed_vertex &&
                          actual.value().failed_cell_x == record.failed_cell_x &&
                          actual.value().failed_cell_z == record.failed_cell_z &&
                          actual.value().conflicts == record.conflicts;
            } else {
                matched = !actual.ok() && actual.error().code == record.status;
            }
        }
        const Outcome<void> released = store->release(handle);
        if (!matched) {
            return Outcome<ReplayReport>::failure(trace_error(
                "replayed query result differs from trace",
                "trace.replay." + std::to_string(record.sequence)));
        }
        if (!released) {
            return Outcome<ReplayReport>::failure(released.error());
        }
        ++report.records;
    }
    report.final_hash = trace.final_hash;
    return Outcome<ReplayReport>::success(std::move(report));
}

Outcome<ReplayReport> replay_trace_files(const std::string& trace_path,
                                         const std::string& snapshot_directory) {
    Outcome<TraceFile> trace = load_trace(trace_path);
    if (!trace) {
        return Outcome<ReplayReport>::failure(trace.error());
    }
    Outcome<std::vector<RegionState>> snapshots =
        load_snapshot_directory(snapshot_directory);
    if (!snapshots) {
        return Outcome<ReplayReport>::failure(snapshots.error());
    }
    return replay_trace(trace.value(), snapshots.value());
}

}  // namespace battle
