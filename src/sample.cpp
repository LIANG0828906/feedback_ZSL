#include "battle/sample.h"

#include "battle/bits.h"
#include "battle/region.h"
#include "battle/sha256.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <charconv>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iterator>
#include <optional>
#include <sstream>
#include <system_error>

namespace battle {
namespace {

namespace fs = std::filesystem;

struct Rng {
    std::uint64_t state = 0;

    explicit Rng(std::uint64_t seed) : state(seed) {}

    std::uint64_t next() {
        state += 0x9e3779b97f4a7c15ULL;
        std::uint64_t value = state;
        value = (value ^ (value >> 30U)) * 0xbf58476d1ce4e5b9ULL;
        value = (value ^ (value >> 27U)) * 0x94d049bb133111ebULL;
        return value ^ (value >> 31U);
    }

    float unit_float() {
        return static_cast<float>(next() >> 40U) * (1.0F / 16777216.0F);
    }

    float range(float low, float high) {
        return low + (high - low) * unit_float();
    }

    std::uint32_t range_u32(std::uint32_t low, std::uint32_t high) {
        if (high <= low) {
            return low;
        }
        return low + static_cast<std::uint32_t>(
                         next() % static_cast<std::uint64_t>(high - low));
    }
};

Error sample_error(StatusCode code, std::string message, std::string point) {
    Error error;
    error.code = code;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

std::string field_bits(const float* values, std::size_t count) {
    std::string result;
    for (std::size_t i = 0; i < count; ++i) {
        if (i != 0) {
            result.push_back(',');
        }
        result += hex32(float_bits(values[i]));
    }
    return result;
}

std::string camera_bits(const Camera& camera) {
    const float values[15] = {
        camera.position.x, camera.position.y, camera.position.z,
        camera.forward.x, camera.forward.y, camera.forward.z,
        camera.up.x, camera.up.y, camera.up.z,
        camera.fov_y_radians, camera.aspect,
        camera.viewport_width, camera.viewport_height,
        camera.near_plane, camera.far_plane};
    return field_bits(values, 15);
}

bool parse_camera_bits(const std::string& text, Camera& camera) {
    const std::vector<std::string> tokens = [&]() {
        std::vector<std::string> out;
        std::size_t start = 0;
        while (true) {
            const std::size_t position = text.find(',', start);
            if (position == std::string::npos) {
                out.push_back(text.substr(start));
                break;
            }
            out.push_back(text.substr(start, position - start));
            start = position + 1;
        }
        return out;
    }();
    if (tokens.size() != 15) {
        return false;
    }
    std::vector<float> values;
    values.reserve(15);
    for (const std::string& token : tokens) {
        std::uint32_t bits = 0;
        if (!parse_hex32(token, bits)) {
            return false;
        }
        values.push_back(bits_float(bits));
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

bool parse_float_bits_list(const std::string& text, std::vector<float>& values,
                           std::size_t expected) {
    std::vector<std::string> tokens;
    std::size_t start = 0;
    while (true) {
        const std::size_t position = text.find(',', start);
        if (position == std::string::npos) {
            tokens.push_back(text.substr(start));
            break;
        }
        tokens.push_back(text.substr(start, position - start));
        start = position + 1;
    }
    if (tokens.size() != expected) {
        return false;
    }
    values.clear();
    for (const std::string& token : tokens) {
        std::uint32_t bits = 0;
        if (!parse_hex32(token, bits)) {
            return false;
        }
        values.push_back(bits_float(bits));
    }
    return true;
}

std::string pick_operation(const Camera& camera,
                           const std::optional<glm::vec2>& screen) {
    ScreenPoint point = {camera.viewport_width * 0.5F,
                         camera.viewport_height * 0.5F};
    if (screen.has_value()) {
        point = {screen->x, screen->y};
    }
    const float values[2] = {point.x, point.y};
    return "PICK " + field_bits(values, 2);
}

std::string box_operation(float min_x, float min_y, float max_x, float max_y) {
    const float values[4] = {min_x, min_y, max_x, max_y};
    return "BOX " + field_bits(values, 4);
}

std::string draft_operation(const std::vector<glm::vec3>& vertices) {
    std::string result = "DRAFT " + std::to_string(vertices.size());
    for (const glm::vec3& vertex : vertices) {
        const float values[3] = {vertex.x, vertex.y, vertex.z};
        result.push_back(',');
        result += field_bits(values, 3);
    }
    return result;
}

Outcome<std::optional<glm::vec2>> project_or_empty(const Camera& camera,
                                                   const glm::vec3& point) {
    Outcome<glm::vec2> projected = project_to_screen(camera, point);
    if (!projected) {
        return Outcome<std::optional<glm::vec2>>::success(std::nullopt);
    }
    glm::vec2 screen = projected.value();
    if (screen.x < 1.0F || screen.y < 1.0F ||
        screen.x > camera.viewport_width - 1.0F ||
        screen.y > camera.viewport_height - 1.0F) {
        return Outcome<std::optional<glm::vec2>>::success(std::nullopt);
    }
    return Outcome<std::optional<glm::vec2>>::success(screen);
}

std::vector<std::string> split_whitespace(const std::string& text) {
    std::istringstream stream(text);
    std::vector<std::string> tokens;
    std::string token;
    while (stream >> token) {
        tokens.push_back(token);
    }
    return tokens;
}

bool parse_u32_text(const std::string& text, std::uint32_t& value) {
    if (text.empty()) {
        return false;
    }
    std::uint64_t parsed = 0;
    const auto result = std::from_chars(
        text.data(), text.data() + text.size(), parsed, 10);
    if (result.ec != std::errc() ||
        result.ptr != text.data() + text.size() || parsed > 0xffffffffULL) {
        return false;
    }
    value = static_cast<std::uint32_t>(parsed);
    return true;
}

Outcome<std::vector<std::string>> read_operations(const std::string& path) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        return Outcome<std::vector<std::string>>::failure(sample_error(
            StatusCode::IoError, "cannot open operations file: " + path,
            "ops.file.open"));
    }
    std::vector<std::string> lines;
    std::string line;
    std::uint64_t line_number = 0;
    while (std::getline(stream, line)) {
        ++line_number;
        if (!line.empty() && line.back() == '\r') {
            line.pop_back();
        }
        const std::size_t start = line.find_first_not_of(" \t");
        if (start == std::string::npos || line[start] == '#') {
            continue;
        }
        lines.push_back(line);
    }
    return Outcome<std::vector<std::string>>::success(std::move(lines));
}

std::string snapshot_filename(std::uint64_t version) {
    std::ostringstream name;
    name << "v" << std::setw(12) << std::setfill('0') << version << ".region";
    return name.str();
}

Outcome<void> prepare_snapshot_directory(const fs::path& directory) {
    std::error_code error;
    fs::create_directories(directory, error);
    if (error) {
        return Outcome<void>::failure(sample_error(
            StatusCode::IoError, "cannot create snapshot directory",
            "snapshots.directory.create"));
    }
    for (const fs::directory_entry& entry : fs::directory_iterator(directory, error)) {
        if (error) {
            return Outcome<void>::failure(sample_error(
                StatusCode::IoError, "cannot enumerate snapshot directory",
                "snapshots.directory.iterate"));
        }
        if (entry.is_regular_file() && entry.path().extension() == ".region") {
            fs::remove(entry.path(), error);
            if (error) {
                return Outcome<void>::failure(sample_error(
                    StatusCode::IoError, "cannot clear old snapshot",
                    "snapshots.directory.clear"));
            }
        }
    }
    return Outcome<void>::success();
}

Outcome<void> save_snapshot_version(const fs::path& directory,
                                    const RegionState& state) {
    return save_region_snapshot(
        (directory / snapshot_filename(state.version)).string(), state);
}

struct SampleCase {
    Scene scene;
    std::vector<std::string> operations;
};

Outcome<SampleCase> make_case(std::uint64_t seed, std::uint32_t case_index) {
    const std::uint32_t dimensions[3][3] = {
        {16, 4, 12},
        {24, 4, 18},
        {40, 4, 28}};
    const std::uint32_t units_per_case[3] = {6, 14, 28};
    const std::uint32_t covers_per_case[3] = {4, 8, 14};
    const std::uint32_t index = case_index % 3;
    Rng rng(seed ^ (0xd1b54a32d192ed03ULL *
                    (static_cast<std::uint64_t>(case_index) + 1ULL)));

    SampleCase result;
    Scene& scene = result.scene;
    scene.grid.width = dimensions[index][0];
    scene.grid.levels = dimensions[index][1];
    scene.grid.depth = dimensions[index][2];
    scene.grid.cell_size = 1.0F;
    scene.grid.origin = {0.0F, 0.0F, 0.0F};
    scene.grid.walkable.assign(
        static_cast<std::size_t>(scene.grid.cell_count()), 1U);
    for (std::uint32_t z = scene.grid.depth - 4; z < scene.grid.depth - 1; ++z) {
        for (std::uint32_t x = scene.grid.width - 4; x < scene.grid.width - 1;
             ++x) {
            scene.grid.walkable[scene.grid.index(x, 0, z)] = 0U;
        }
    }

    for (std::uint32_t i = 0; i < units_per_case[index]; ++i) {
        const float x = rng.range(2.0F, static_cast<float>(scene.grid.width) * 0.58F);
        const float z = rng.range(2.0F, static_cast<float>(scene.grid.depth) - 2.0F);
        Unit unit;
        unit.id = i + 1;
        if ((i % 2U) == 0U) {
            unit.shape = UnitShape::Aabb;
            const float half_x = rng.range(0.35F, 0.65F);
            const float half_z = rng.range(0.35F, 0.65F);
            const float bottom = rng.range(0.05F, 0.35F);
            const float top = bottom + rng.range(0.7F, 1.5F);
            unit.aabb.min = {x - half_x, bottom, z - half_z};
            unit.aabb.max = {x + half_x, top, z + half_z};
        } else {
            unit.shape = UnitShape::Capsule;
            const float bottom = rng.range(0.15F, 0.4F);
            unit.capsule.a = {x, bottom, z};
            unit.capsule.b = {x, bottom + rng.range(0.8F, 1.6F), z};
            unit.capsule.radius = rng.range(0.22F, 0.48F);
        }
        scene.units.push_back(unit);
    }

    for (std::uint32_t i = 0; i < covers_per_case[index]; ++i) {
        const float x = rng.range(
            static_cast<float>(scene.grid.width) * 0.42F,
            static_cast<float>(scene.grid.width) - 2.5F);
        const float z = rng.range(2.0F, static_cast<float>(scene.grid.depth) - 2.5F);
        const float half = rng.range(0.45F, 1.1F);
        const float angle = rng.range(0.0F, 3.141592653F);
        const float dx = std::cos(angle) * half;
        const float dz = std::sin(angle) * half;
        Cover cover;
        cover.id = 1001U + i;
        cover.present = true;
        cover.triangle.a = {x - dx, 0.04F, z - dz};
        cover.triangle.b = {x + dx, 0.04F, z + dz};
        cover.triangle.c = {x + dz * 0.25F, rng.range(0.8F, 1.9F),
                            z - dx * 0.25F};
        scene.covers.push_back(cover);
    }

    Outcome<void> valid = scene.validate();
    if (!valid) {
        return Outcome<SampleCase>::failure(valid.error());
    }

    const Unit& first_unit = scene.units.front();
    const glm::vec3 first_center = aabb_center(unit_bounds(first_unit));
    Camera camera;
    if (index == 0) {
        camera = make_look_at_camera(
            {first_center.x - 3.0F, 13.0F,
             static_cast<float>(scene.grid.depth) + 8.0F},
            first_center, 0.785398163F, 16.0F / 9.0F, 1280.0F, 720.0F);
    } else if (index == 1) {
        camera = make_look_at_camera(
            {static_cast<float>(scene.grid.width) + 9.0F, 17.0F,
             static_cast<float>(scene.grid.depth) + 3.0F},
            {static_cast<float>(scene.grid.width) * 0.45F, 0.0F,
             static_cast<float>(scene.grid.depth) * 0.45F},
            0.65F, 16.0F / 9.0F, 1280.0F, 720.0F);
    } else {
        camera = make_look_at_camera(
            {-5.0F, 21.0F, static_cast<float>(scene.grid.depth) + 15.0F},
            {static_cast<float>(scene.grid.width) * 0.55F, 0.0F,
             static_cast<float>(scene.grid.depth) * 0.4F},
            0.95F, 16.0F / 9.0F, 1280.0F, 720.0F);
    }
    result.operations.push_back("CAMERA " + camera_bits(camera));

    auto append_pick = [&](const glm::vec3& target) {
        Outcome<std::optional<glm::vec2>> screen = project_or_empty(camera, target);
        if (!screen) {
            return;
        }
        result.operations.push_back(pick_operation(camera, screen.value()));
    };

    append_pick(first_center);
    if (scene.covers.size() > 1) {
        const Cover& cover = scene.covers[1];
        append_pick((cover.triangle.a + cover.triangle.b + cover.triangle.c) /
                    3.0F);
    }
    result.operations.push_back(box_operation(
        camera.viewport_width * 0.30F, camera.viewport_height * 0.25F,
        camera.viewport_width * 0.72F, camera.viewport_height * 0.82F));

    RegionState state = RegionState::from_scene(scene);
    std::vector<glm::vec3> legal_region;
    {
        Outcome<std::shared_ptr<RegionStore>> store = RegionStore::create(state);
        if (!store) {
            return Outcome<SampleCase>::failure(store.error());
        }
        Outcome<SnapshotHandle> handle = store.value()->acquire();
        if (!handle) {
            return Outcome<SampleCase>::failure(handle.error());
        }
        const float size = scene.grid.cell_size * 0.8F;
        for (std::uint32_t z = 1; z < scene.grid.depth - 1 && legal_region.empty();
             ++z) {
            for (std::uint32_t x = 1; x < scene.grid.width - 1 &&
                                      legal_region.empty();
                 ++x) {
                const glm::vec3 center{
                    scene.grid.origin.x +
                        (static_cast<float>(x) + 0.5F) * scene.grid.cell_size,
                    scene.grid.ground_y(),
                    scene.grid.origin.z +
                        (static_cast<float>(z) + 0.5F) * scene.grid.cell_size};
                PlacementRequest request;
                request.region.vertices = {
                    {center.x - size, center.y, center.z - size},
                    {center.x + size, center.y, center.z - size},
                    {center.x + size, center.y, center.z + size},
                    {center.x - size, center.y, center.z + size}};
                Outcome<PlacementResult> placed = validate_placement(
                    *store.value(), handle.value(), request);
                if (placed && placed.value().legal) {
                    legal_region = request.region.vertices;
                }
            }
        }
        Outcome<void> released = store.value()->release(handle.value());
        if (!released) {
            return Outcome<SampleCase>::failure(released.error());
        }
    }
    if (!legal_region.empty()) {
        result.operations.push_back(draft_operation(legal_region));
    }

    {
        const Aabb bounds = unit_bounds(scene.units.front());
        const std::vector<glm::vec3> overlap = {
            {bounds.min.x - 0.05F, scene.grid.ground_y(), bounds.min.z - 0.05F},
            {bounds.max.x + 0.05F, scene.grid.ground_y(), bounds.min.z - 0.05F},
            {bounds.max.x + 0.05F, scene.grid.ground_y(), bounds.max.z + 0.05F},
            {bounds.min.x - 0.05F, scene.grid.ground_y(), bounds.max.z + 0.05F}};
        result.operations.push_back(draft_operation(overlap));
    }
    {
        const float cx = static_cast<float>(scene.grid.width) * 0.5F;
        const float cz = static_cast<float>(scene.grid.depth) * 0.5F;
        const float half = scene.grid.cell_size * 0.75F;
        result.operations.push_back(draft_operation({
            {cx - half, scene.grid.ground_y(), cz - half},
            {cx + half, scene.grid.ground_y(), cz + half},
            {cx + half, scene.grid.ground_y(), cz - half},
            {cx - half, scene.grid.ground_y(), cz + half}}));
    }

    {
        UnitMove move;
        move.id = scene.units.front().id;
        move.delta = {0.35F, 0.0F, 0.25F};
        const float delta_values[3] = {
            move.delta.x, move.delta.y, move.delta.z};
        result.operations.push_back(
            "MOVE " + std::to_string(move.id) + " " +
            field_bits(delta_values, 3));
        Outcome<RegionState> moved = apply_region_mutation(state, move);
        if (!moved) {
            return Outcome<SampleCase>::failure(moved.error());
        }
        state = std::move(moved.value());
        const Unit* moved_unit = state.find_unit(move.id);
        if (moved_unit != nullptr) {
            append_pick(aabb_center(unit_bounds(*moved_unit)));
        }
    }

    {
        Camera second = camera;
        second.position += glm::vec3(2.0F, 1.5F, 1.0F);
        second.forward = glm::normalize(
            glm::vec3(static_cast<float>(scene.grid.width) * 0.5F, 0.0F,
                      static_cast<float>(scene.grid.depth) * 0.5F) -
            second.position);
        result.operations.push_back("CAMERA " + camera_bits(second));
        camera = second;
    }

    if (!scene.covers.empty()) {
        const Cover& cover = scene.covers.front();
        CoverChange change;
        change.id = cover.id;
        change.present = false;
        result.operations.push_back(
            "COVER " + std::to_string(change.id) + " 0");
        Outcome<RegionState> changed = apply_region_mutation(state, change);
        if (!changed) {
            return Outcome<SampleCase>::failure(changed.error());
        }
        state = std::move(changed.value());
        append_pick((cover.triangle.a + cover.triangle.b + cover.triangle.c) /
                    3.0F);
    }

    {
        GridChange change;
        change.x = scene.grid.width - 2;
        change.y = 0;
        change.z = scene.grid.depth - 2;
        change.walkable = false;
        result.operations.push_back(
            "WALK " + std::to_string(change.x) + " " +
            std::to_string(change.y) + " " + std::to_string(change.z) + " 0");
        Outcome<RegionState> changed = apply_region_mutation(state, change);
        if (!changed) {
            return Outcome<SampleCase>::failure(changed.error());
        }
        state = std::move(changed.value());
        const float cell_x = scene.grid.origin.x +
                             (static_cast<float>(change.x) + 0.5F) *
                                 scene.grid.cell_size;
        const float cell_z = scene.grid.origin.z +
                             (static_cast<float>(change.z) + 0.5F) *
                                 scene.grid.cell_size;
        const float half = scene.grid.cell_size * 0.22F;
        result.operations.push_back(draft_operation({
            {cell_x - half, scene.grid.ground_y(), cell_z - half},
            {cell_x + half, scene.grid.ground_y(), cell_z - half},
            {cell_x, scene.grid.ground_y(), cell_z + half}}));
    }

    result.operations.push_back(box_operation(
        camera.viewport_width * 0.20F, camera.viewport_height * 0.20F,
        camera.viewport_width * 0.80F, camera.viewport_height * 0.88F));
    return Outcome<SampleCase>::success(std::move(result));
}

Outcome<void> write_text(const fs::path& path, const std::string& text) {
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        return Outcome<void>::failure(sample_error(
            StatusCode::IoError, "cannot write sample file: " + path.string(),
            "sample.file.write"));
    }
    stream.write(text.data(), static_cast<std::streamsize>(text.size()));
    if (!stream) {
        return Outcome<void>::failure(sample_error(
            StatusCode::IoError, "short write for sample file: " + path.string(),
            "sample.file.write"));
    }
    return Outcome<void>::success();
}

Outcome<void> write_binary(const fs::path& path, const std::string& bytes) {
    return write_text(path, bytes);
}

std::string digest_file(const fs::path& path) {
    std::ifstream stream(path, std::ios::binary);
    std::string bytes((std::istreambuf_iterator<char>(stream)),
                      std::istreambuf_iterator<char>());
    return digest_hex(sha256(bytes));
}

}  // namespace

Outcome<RunReport> run_experiment(const std::string& scene_path,
                                  const std::string& operations_path,
                                  const std::string& trace_path,
                                  const std::string& snapshot_directory) {
    Outcome<Scene> loaded = Scene::load_binary(scene_path);
    if (!loaded) {
        return Outcome<RunReport>::failure(loaded.error());
    }
    RegionState initial = RegionState::from_scene(loaded.value());
    Outcome<std::shared_ptr<RegionStore>> created =
        RegionStore::create(initial);
    if (!created) {
        return Outcome<RunReport>::failure(created.error());
    }
    const std::shared_ptr<RegionStore>& store = created.value();
    Outcome<std::vector<std::string>> operations =
        read_operations(operations_path);
    if (!operations) {
        return Outcome<RunReport>::failure(operations.error());
    }

    std::error_code filesystem_error;
    const fs::path snapshot_path(snapshot_directory);
    Outcome<void> prepared = prepare_snapshot_directory(snapshot_path);
    if (!prepared) {
        return Outcome<RunReport>::failure(prepared.error());
    }
    Outcome<void> initial_saved =
        save_snapshot_version(snapshot_path, initial);
    if (!initial_saved) {
        return Outcome<RunReport>::failure(initial_saved.error());
    }
    const fs::path trace_file(trace_path);
    if (trace_file.has_parent_path()) {
        fs::create_directories(trace_file.parent_path(), filesystem_error);
        if (filesystem_error) {
            return Outcome<RunReport>::failure(sample_error(
                StatusCode::IoError, "cannot create trace directory",
                "trace.directory.create"));
        }
    }

    Camera camera;
    TraceRecorder recorder;
    for (std::size_t i = 0; i < operations.value().size(); ++i) {
        const std::string& line = operations.value()[i];
        const std::vector<std::string> tokens = split_whitespace(line);
        if (tokens.empty()) {
            continue;
        }
        const std::string point =
            "ops.line." + std::to_string(i + 1);
        if (tokens[0] == "CAMERA") {
            if (tokens.size() != 2 || !parse_camera_bits(tokens[1], camera)) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid CAMERA operation", point));
            }
        } else if (tokens[0] == "PICK") {
            std::vector<float> values;
            if (tokens.size() != 2 ||
                !parse_float_bits_list(tokens[1], values, 2)) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid PICK operation", point));
            }
            PickRequest request;
            request.camera = camera;
            request.screen = {values[0], values[1]};
            Outcome<SnapshotHandle> acquired = store->acquire();
            if (!acquired) {
                return Outcome<RunReport>::failure(acquired.error());
            }
            Outcome<PickResult> picked =
                pick_screen(*store, acquired.value(), request);
            Ray trace_ray;
            trace_ray.origin = camera.position;
            trace_ray.direction = camera.forward;
            if (picked) {
                trace_ray = picked.value().ray;
            } else {
                Outcome<Ray> generated =
                    screen_to_ray(camera, request.screen);
                if (generated) {
                    trace_ray = generated.value();
                }
            }
            recorder.record_pick(acquired.value().version,
                                 acquired.value().content_hash, request,
                                 trace_ray, picked);
            Outcome<void> released = store->release(acquired.value());
            if (!released) {
                return Outcome<RunReport>::failure(released.error());
            }
        } else if (tokens[0] == "BOX") {
            std::vector<float> values;
            if (tokens.size() != 2 ||
                !parse_float_bits_list(tokens[1], values, 4)) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid BOX operation", point));
            }
            BoxSelectRequest request;
            request.camera = camera;
            request.rect = {values[0], values[1], values[2], values[3]};
            Outcome<SnapshotHandle> acquired = store->acquire();
            if (!acquired) {
                return Outcome<RunReport>::failure(acquired.error());
            }
            Outcome<BoxSelectResult> selected =
                box_select(*store, acquired.value(), request);
            recorder.record_box(acquired.value().version,
                                acquired.value().content_hash, request,
                                selected);
            Outcome<void> released = store->release(acquired.value());
            if (!released) {
                return Outcome<RunReport>::failure(released.error());
            }
        } else if (tokens[0] == "DRAFT") {
            if (tokens.size() != 2) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid DRAFT operation", point));
            }
            const std::vector<std::string> parts = [&]() {
                std::vector<std::string> out;
                std::size_t start = 0;
                while (true) {
                    const std::size_t position = tokens[1].find(',', start);
                    if (position == std::string::npos) {
                        out.push_back(tokens[1].substr(start));
                        break;
                    }
                    out.push_back(tokens[1].substr(start, position - start));
                    start = position + 1;
                }
                return out;
            }();
            std::uint32_t count = 0;
            if (parts.empty() || !parse_u32_text(parts[0], count) ||
                parts.size() != 1U + count * 3U) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid DRAFT vertex list", point));
            }
            PlacementRequest request;
            for (std::uint32_t v = 0; v < count; ++v) {
                std::uint32_t x = 0;
                std::uint32_t y = 0;
                std::uint32_t z = 0;
                if (!parse_hex32(parts[1 + v * 3], x) ||
                    !parse_hex32(parts[2 + v * 3], y) ||
                    !parse_hex32(parts[3 + v * 3], z)) {
                    return Outcome<RunReport>::failure(sample_error(
                        StatusCode::UsageError, "invalid DRAFT vertex", point));
                }
                request.region.vertices.push_back(
                    {bits_float(x), bits_float(y), bits_float(z)});
            }
            Outcome<SnapshotHandle> acquired = store->acquire();
            if (!acquired) {
                return Outcome<RunReport>::failure(acquired.error());
            }
            Outcome<PlacementResult> placement =
                validate_placement(*store, acquired.value(), request);
            recorder.record_placement(acquired.value().version,
                                      acquired.value().content_hash, request,
                                      placement);
            Outcome<void> released = store->release(acquired.value());
            if (!released) {
                return Outcome<RunReport>::failure(released.error());
            }
        } else if (tokens[0] == "WALK") {
            GridChange change;
            if (tokens.size() != 5 ||
                !parse_u32_text(tokens[1], change.x) ||
                !parse_u32_text(tokens[2], change.y) ||
                !parse_u32_text(tokens[3], change.z) ||
                (tokens[4] != "0" && tokens[4] != "1")) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid WALK operation", point));
            }
            change.walkable = tokens[4] == "1";
            RegionState current = store->current_state_copy();
            Outcome<RegionState> next = apply_region_mutation(current, change);
            if (!next) {
                return Outcome<RunReport>::failure(next.error());
            }
            Outcome<PublishedRegionPtr> published = store->publish(next.value());
            if (!published) {
                return Outcome<RunReport>::failure(published.error());
            }
            Outcome<void> saved =
                save_snapshot_version(snapshot_path, next.value());
            if (!saved) {
                return Outcome<RunReport>::failure(saved.error());
            }
        } else if (tokens[0] == "MOVE") {
            UnitMove move;
            std::vector<float> values;
            if (tokens.size() != 3 || !parse_u32_text(tokens[1], move.id) ||
                !parse_float_bits_list(tokens[2], values, 3)) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid MOVE operation", point));
            }
            move.delta = {values[0], values[1], values[2]};
            RegionState current = store->current_state_copy();
            Outcome<RegionState> next = apply_region_mutation(current, move);
            if (!next) {
                return Outcome<RunReport>::failure(next.error());
            }
            Outcome<PublishedRegionPtr> published = store->publish(next.value());
            if (!published) {
                return Outcome<RunReport>::failure(published.error());
            }
            Outcome<void> saved =
                save_snapshot_version(snapshot_path, next.value());
            if (!saved) {
                return Outcome<RunReport>::failure(saved.error());
            }
        } else if (tokens[0] == "COVER") {
            CoverChange change;
            if (tokens.size() != 3 ||
                !parse_u32_text(tokens[1], change.id) ||
                (tokens[2] != "0" && tokens[2] != "1")) {
                return Outcome<RunReport>::failure(sample_error(
                    StatusCode::UsageError, "invalid COVER operation", point));
            }
            change.present = tokens[2] == "1";
            RegionState current = store->current_state_copy();
            Outcome<RegionState> next = apply_region_mutation(current, change);
            if (!next) {
                return Outcome<RunReport>::failure(next.error());
            }
            Outcome<PublishedRegionPtr> published = store->publish(next.value());
            if (!published) {
                return Outcome<RunReport>::failure(published.error());
            }
            Outcome<void> saved =
                save_snapshot_version(snapshot_path, next.value());
            if (!saved) {
                return Outcome<RunReport>::failure(saved.error());
            }
        } else {
            return Outcome<RunReport>::failure(sample_error(
                StatusCode::UsageError,
                "unknown operation: " + tokens[0], point));
        }
    }

    Outcome<void> written = recorder.write_file(trace_path);
    if (!written) {
        return Outcome<RunReport>::failure(written.error());
    }
    RunReport report;
    report.final_version = store->current_version();
    report.records = static_cast<std::uint64_t>(recorder.size());
    report.final_trace_hash = recorder.final_hash();
    return Outcome<RunReport>::success(std::move(report));
}

Outcome<GenerationReport> generate_samples(std::uint64_t seed,
                                           const std::string& output_directory,
                                           std::uint32_t case_count) {
    if (case_count == 0 || case_count > 16) {
        return Outcome<GenerationReport>::failure(sample_error(
            StatusCode::UsageError, "case count must be between 1 and 16",
            "sample.case_count"));
    }
    const fs::path root(output_directory);
    std::error_code error;
    fs::create_directories(root, error);
    if (error) {
        return Outcome<GenerationReport>::failure(sample_error(
            StatusCode::IoError, "cannot create sample output directory",
            "sample.output.create"));
    }
    for (const fs::directory_entry& entry : fs::directory_iterator(root, error)) {
        if (error) {
            return Outcome<GenerationReport>::failure(sample_error(
                StatusCode::IoError, "cannot enumerate sample output directory",
                "sample.output.iterate"));
        }
        const std::string name = entry.path().filename().string();
        if (entry.is_directory() && name.rfind("case-", 0) == 0) {
            fs::remove_all(entry.path(), error);
            if (error) {
                return Outcome<GenerationReport>::failure(sample_error(
                    StatusCode::IoError, "cannot clear old sample case",
                    "sample.output.clear"));
            }
        }
    }

    GenerationReport report;
    std::ostringstream manifest;
    manifest << "BATTLE_SAMPLES|seed=" << seed << "|cases=" << case_count << '\n';
    for (std::uint32_t i = 0; i < case_count; ++i) {
        Outcome<SampleCase> generated = make_case(seed, i);
        if (!generated) {
            return Outcome<GenerationReport>::failure(generated.error());
        }
        std::ostringstream directory_name;
        directory_name << "case-" << std::setw(3) << std::setfill('0') << i;
        const fs::path case_directory = root / directory_name.str();
        fs::create_directories(case_directory, error);
        if (error) {
            return Outcome<GenerationReport>::failure(sample_error(
                StatusCode::IoError, "cannot create case directory",
                "sample.case.directory"));
        }
        const fs::path scene_path = case_directory / "scene.bin";
        const fs::path operations_path = case_directory / "ops.txt";
        Outcome<void> scene_written =
            write_binary(scene_path, generated.value().scene.encode_binary());
        if (!scene_written) {
            return Outcome<GenerationReport>::failure(scene_written.error());
        }
        std::string operations;
        for (const std::string& line : generated.value().operations) {
            operations += line;
            operations.push_back('\n');
        }
        Outcome<void> ops_written = write_text(operations_path, operations);
        if (!ops_written) {
            return Outcome<GenerationReport>::failure(ops_written.error());
        }
        const fs::path baseline_directory = case_directory / "baseline";
        fs::create_directories(baseline_directory, error);
        if (error) {
            return Outcome<GenerationReport>::failure(sample_error(
                StatusCode::IoError, "cannot create baseline directory",
                "sample.baseline.directory"));
        }
        const fs::path baseline_trace = baseline_directory / "trace.log";
        const fs::path snapshots = case_directory / "snapshots";
        Outcome<RunReport> run = run_experiment(
            scene_path.string(), operations_path.string(),
            baseline_trace.string(), snapshots.string());
        if (!run) {
            return Outcome<GenerationReport>::failure(run.error());
        }
        const std::string scene_digest = digest_file(scene_path);
        const std::string trace_digest = digest_file(baseline_trace);
        manifest << directory_name.str()
                 << "|version=" << run.value().final_version
                 << "|records=" << run.value().records
                 << "|scene=" << scene_digest
                 << "|trace=" << trace_digest
                 << "|chain=" << run.value().final_trace_hash << '\n';
        report.cases.push_back(directory_name.str());
    }
    const std::string manifest_text = manifest.str();
    Outcome<void> manifest_written =
        write_text(root / "manifest.txt", manifest_text);
    if (!manifest_written) {
        return Outcome<GenerationReport>::failure(manifest_written.error());
    }
    report.manifest_hash = digest_hex(sha256(manifest_text));
    return Outcome<GenerationReport>::success(std::move(report));
}

}  // namespace battle
