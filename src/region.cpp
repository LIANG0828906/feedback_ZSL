#include "battle/region.h"

#include "battle/binary.h"
#include "battle/sha256.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <fstream>
#include <iterator>
#include <set>
#include <sstream>

namespace battle {
namespace {

constexpr char kSnapshotMagic[8] = {'B', 'T', 'S', 'N', 'A', 'P', '0', '1'};
constexpr std::uint32_t kSnapshotEnd = 0x444E4553U;  // "SEND"

Error region_error(StatusCode code, std::string message, std::string point) {
    Error error;
    error.code = code;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

void write_unit(ByteWriter& writer, const Unit& unit) {
    writer.u32(unit.id);
    writer.u8(static_cast<std::uint8_t>(unit.shape));
    writer.u8(0);
    writer.u8(0);
    writer.u8(0);
    if (unit.shape == UnitShape::Aabb) {
        writer.vec3(unit.aabb.min);
        writer.vec3(unit.aabb.max);
    } else {
        writer.vec3(unit.capsule.a);
        writer.vec3(unit.capsule.b);
        writer.f32(unit.capsule.radius);
    }
}

bool read_unit(ByteReader& reader, Unit& unit) {
    std::uint8_t shape = 0;
    std::uint8_t reserved[3]{};
    if (!reader.u32(unit.id) || !reader.u8(shape) ||
        !reader.bytes(reserved, sizeof(reserved))) {
        return false;
    }
    if (reserved[0] != 0 || reserved[1] != 0 || reserved[2] != 0) {
        return false;
    }
    unit.shape = static_cast<UnitShape>(shape);
    if (unit.shape == UnitShape::Aabb) {
        return reader.vec3(unit.aabb.min) && reader.vec3(unit.aabb.max);
    }
    if (unit.shape == UnitShape::Capsule) {
        return reader.vec3(unit.capsule.a) && reader.vec3(unit.capsule.b) &&
               reader.f32(unit.capsule.radius);
    }
    return false;
}

Outcome<void> validate_state_geometry(const RegionState& state) {
    Scene scene;
    scene.grid = state.grid;
    scene.units = state.units;
    scene.covers = state.covers;
    return scene.validate();
}

void sort_entities(RegionState& state) {
    std::sort(state.units.begin(), state.units.end(),
              [](const Unit& lhs, const Unit& rhs) { return lhs.id < rhs.id; });
    std::sort(state.covers.begin(), state.covers.end(),
              [](const Cover& lhs, const Cover& rhs) { return lhs.id < rhs.id; });
}

void append_bvh_hash(Sha256Builder& builder, const Bvh& bvh) {
    builder.append_u64(bvh.primitive_bounds().size());
    for (const Aabb& bounds : bvh.primitive_bounds()) {
        builder.append_f32_bits(bounds.min.x);
        builder.append_f32_bits(bounds.min.y);
        builder.append_f32_bits(bounds.min.z);
        builder.append_f32_bits(bounds.max.x);
        builder.append_f32_bits(bounds.max.y);
        builder.append_f32_bits(bounds.max.z);
    }
    builder.append_u64(bvh.nodes().size());
    for (const Bvh::Node& node : bvh.nodes()) {
        builder.append_f32_bits(node.bounds.min.x);
        builder.append_f32_bits(node.bounds.min.y);
        builder.append_f32_bits(node.bounds.min.z);
        builder.append_f32_bits(node.bounds.max.x);
        builder.append_f32_bits(node.bounds.max.y);
        builder.append_f32_bits(node.bounds.max.z);
        builder.append_u32(node.left);
        builder.append_u32(node.right);
        builder.append_u32(node.start);
        builder.append_u32(node.count);
        builder.append_u8(node.leaf ? 1U : 0U);
    }
}

}  // namespace

RegionState RegionState::from_scene(const Scene& scene) {
    RegionState state;
    state.version = 1;
    state.grid = scene.grid;
    state.units = scene.units;
    state.covers = scene.covers;
    sort_entities(state);
    for (Cover& cover : state.covers) {
        cover.present = true;
    }
    state.seal();
    return state;
}

std::string encode_region_payload(const RegionState& state) {
    ByteWriter writer;
    writer.u64(state.version);
    writer.u32(state.grid.width);
    writer.u32(state.grid.levels);
    writer.u32(state.grid.depth);
    writer.f32(state.grid.cell_size);
    writer.vec3(state.grid.origin);
    writer.u32(static_cast<std::uint32_t>(state.grid.walkable.size()));
    writer.bytes(state.grid.walkable.data(), state.grid.walkable.size());
    writer.u32(static_cast<std::uint32_t>(state.units.size()));
    for (const Unit& unit : state.units) {
        write_unit(writer, unit);
    }
    writer.u32(static_cast<std::uint32_t>(state.covers.size()));
    for (const Cover& cover : state.covers) {
        writer.u32(cover.id);
        writer.u8(cover.present ? 1U : 0U);
        writer.u8(0);
        writer.u8(0);
        writer.u8(0);
        writer.vec3(cover.triangle.a);
        writer.vec3(cover.triangle.b);
        writer.vec3(cover.triangle.c);
    }
    return writer.take();
}

Outcome<RegionState> decode_region_payload(const std::string& payload) {
    ByteReader reader(payload);
    RegionState state;
    std::uint32_t walkable_count = 0;
    std::uint32_t unit_count = 0;
    std::uint32_t cover_count = 0;
    if (!reader.u64(state.version) ||
        !reader.u32(state.grid.width) ||
        !reader.u32(state.grid.levels) ||
        !reader.u32(state.grid.depth) ||
        !reader.f32(state.grid.cell_size) ||
        !reader.vec3(state.grid.origin) ||
        !reader.u32(walkable_count)) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "truncated region payload header",
            "snapshot.payload.grid"));
    }
    if (state.grid.cell_count() != walkable_count) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale,
            "snapshot walkability count does not match grid",
            "snapshot.payload.walkable"));
    }
    state.grid.walkable.resize(walkable_count);
    if (!state.grid.walkable.empty() &&
        !reader.bytes(state.grid.walkable.data(), state.grid.walkable.size())) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "truncated walkability payload",
            "snapshot.payload.walkable"));
    }
    if (!reader.u32(unit_count)) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "missing unit count",
            "snapshot.payload.units"));
    }
    state.units.reserve(unit_count);
    for (std::uint32_t i = 0; i < unit_count; ++i) {
        Unit unit;
        if (!read_unit(reader, unit)) {
            return Outcome<RegionState>::failure(region_error(
                StatusCode::SnapshotStale, "truncated unit payload",
                "snapshot.payload.unit"));
        }
        state.units.push_back(unit);
    }
    if (!reader.u32(cover_count)) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "missing cover count",
            "snapshot.payload.covers"));
    }
    state.covers.reserve(cover_count);
    for (std::uint32_t i = 0; i < cover_count; ++i) {
        Cover cover;
        std::uint8_t present = 0;
        std::uint8_t reserved[3]{};
        if (!reader.u32(cover.id) || !reader.u8(present) ||
            !reader.bytes(reserved, sizeof(reserved)) ||
            !reader.vec3(cover.triangle.a) ||
            !reader.vec3(cover.triangle.b) ||
            !reader.vec3(cover.triangle.c) ||
            present > 1 || reserved[0] != 0 || reserved[1] != 0 ||
            reserved[2] != 0) {
            return Outcome<RegionState>::failure(region_error(
                StatusCode::SnapshotStale, "invalid cover payload",
                "snapshot.payload.cover"));
        }
        cover.present = present != 0;
        state.covers.push_back(cover);
    }
    if (!reader.finished()) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "trailing region payload bytes",
            "snapshot.payload.trailing"));
    }
    sort_entities(state);
    state.seal();
    Outcome<void> valid = validate_state_geometry(state);
    if (!valid) {
        return Outcome<RegionState>::failure(valid.error());
    }
    return Outcome<RegionState>::success(std::move(state));
}

Digest RegionState::compute_digest() const {
    return sha256(encode_region_payload(*this));
}

std::string RegionState::compute_hash() const {
    return digest_hex(compute_digest());
}

void RegionState::seal() {
    content_hash = compute_hash();
}

const Unit* RegionState::find_unit(std::uint32_t id) const {
    const auto it = std::lower_bound(
        units.begin(), units.end(), id,
        [](const Unit& unit, std::uint32_t wanted) { return unit.id < wanted; });
    return it != units.end() && it->id == id ? &*it : nullptr;
}

const Cover* RegionState::find_cover(std::uint32_t id) const {
    const auto it = std::lower_bound(
        covers.begin(), covers.end(), id,
        [](const Cover& cover, std::uint32_t wanted) { return cover.id < wanted; });
    return it != covers.end() && it->id == id ? &*it : nullptr;
}

std::string compute_acceleration_hash(const Bvh& unit_bvh,
                                      const Bvh& cover_bvh) {
    Sha256Builder builder;
    append_bvh_hash(builder, unit_bvh);
    append_bvh_hash(builder, cover_bvh);
    return digest_hex(builder.finish());
}

Outcome<PublishedRegionPtr> build_published_region(const RegionState& state) {
    Outcome<void> valid = validate_state_geometry(state);
    if (!valid) {
        return Outcome<PublishedRegionPtr>::failure(valid.error());
    }
    const std::string computed = state.compute_hash();
    if (state.content_hash != computed) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::RegionStale,
            "region content hash does not match canonical state",
            "region.hash"));
    }

    auto published = std::make_shared<PublishedRegion>();
    published->state = state;
    std::vector<Aabb> unit_bounds;
    unit_bounds.reserve(state.units.size());
    for (const Unit& unit : state.units) {
        unit_bounds.push_back(battle::unit_bounds(unit));
    }
    published->unit_bvh = Bvh::build(unit_bounds);
    std::vector<Aabb> cover_bounds;
    cover_bounds.reserve(state.covers.size());
    for (const Cover& cover : state.covers) {
        cover_bounds.push_back(triangle_bounds(cover.triangle));
    }
    published->cover_bvh = Bvh::build(cover_bounds);
    published->acceleration_hash =
        compute_acceleration_hash(published->unit_bvh, published->cover_bvh);
    return Outcome<PublishedRegionPtr>::success(std::move(published));
}

Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const GridChange* grid_change,
                                           const UnitMove* unit_move,
                                           const CoverChange* cover_change) {
    RegionState next = base;
    next.version = base.version + 1;

    if (grid_change != nullptr) {
        if (!next.grid.contains_cell(grid_change->x, grid_change->y,
                                     grid_change->z)) {
            return Outcome<RegionState>::failure(region_error(
                StatusCode::SceneInvalid, "grid mutation is outside the grid",
                "region.mutation.grid"));
        }
        next.grid.walkable[next.grid.index(grid_change->x, grid_change->y,
                                           grid_change->z)] =
            grid_change->walkable ? 1U : 0U;
    }

    if (unit_move != nullptr) {
        const auto it = std::lower_bound(
            next.units.begin(), next.units.end(), unit_move->id,
            [](const Unit& unit, std::uint32_t id) { return unit.id < id; });
        if (it == next.units.end() || it->id != unit_move->id) {
            Error error = region_error(
                StatusCode::SceneInvalid, "unit mutation references an unknown unit",
                "region.mutation.unit");
            error.entity = {EntityKind::Unit, unit_move->id};
            return Outcome<RegionState>::failure(std::move(error));
        }
        if (!is_finite(unit_move->delta)) {
            Error error = region_error(
                StatusCode::SceneInvalid, "unit translation must be finite",
                "region.mutation.unit.delta");
            error.entity = {EntityKind::Unit, unit_move->id};
            return Outcome<RegionState>::failure(std::move(error));
        }
        if (it->shape == UnitShape::Aabb) {
            it->aabb.min += unit_move->delta;
            it->aabb.max += unit_move->delta;
        } else {
            it->capsule.a += unit_move->delta;
            it->capsule.b += unit_move->delta;
        }
    }

    if (cover_change != nullptr) {
        const auto it = std::lower_bound(
            next.covers.begin(), next.covers.end(), cover_change->id,
            [](const Cover& cover, std::uint32_t id) { return cover.id < id; });
        if (it == next.covers.end() || it->id != cover_change->id) {
            Error error = region_error(
                StatusCode::SceneInvalid,
                "cover mutation references an unknown cover",
                "region.mutation.cover");
            error.entity = {EntityKind::Cover, cover_change->id};
            return Outcome<RegionState>::failure(std::move(error));
        }
        it->present = cover_change->present;
    }

    sort_entities(next);
    next.seal();
    return Outcome<RegionState>::success(std::move(next));
}

Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const GridChange& grid_change) {
    return apply_region_mutation(base, &grid_change, nullptr, nullptr);
}

Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const UnitMove& unit_move) {
    return apply_region_mutation(base, nullptr, &unit_move, nullptr);
}

Outcome<RegionState> apply_region_mutation(const RegionState& base,
                                           const CoverChange& cover_change) {
    return apply_region_mutation(base, nullptr, nullptr, &cover_change);
}

Outcome<void> save_region_snapshot(const std::string& path,
                                   const RegionState& state) {
    const std::string payload = encode_region_payload(state);
    Digest digest = state.compute_digest();
    ByteWriter writer;
    writer.bytes(kSnapshotMagic, sizeof(kSnapshotMagic));
    writer.u32(static_cast<std::uint32_t>(kSnapshotFormatVersion));
    writer.u64(state.version);
    writer.bytes(digest.data(), digest.size());
    writer.u64(payload.size());
    writer.bytes(payload);
    writer.u32(kSnapshotEnd);

    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        return Outcome<void>::failure(region_error(
            StatusCode::IoError, "cannot open snapshot for writing: " + path,
            "snapshot.file.open"));
    }
    const std::string bytes = writer.data();
    stream.write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
    if (!stream) {
        return Outcome<void>::failure(region_error(
            StatusCode::IoError, "cannot write snapshot: " + path,
            "snapshot.file.write"));
    }
    return Outcome<void>::success();
}

Outcome<RegionState> load_region_snapshot(const std::string& path) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::IoError, "cannot open snapshot: " + path,
            "snapshot.file.open"));
    }
    std::string bytes((std::istreambuf_iterator<char>(stream)),
                      std::istreambuf_iterator<char>());
    ByteReader reader(bytes);
    char magic[8]{};
    std::uint32_t format = 0;
    std::uint64_t version = 0;
    Digest stored_digest{};
    std::uint64_t payload_size = 0;
    if (!reader.bytes(magic, sizeof(magic)) ||
        !std::equal(std::begin(kSnapshotMagic), std::end(kSnapshotMagic),
                    std::begin(magic)) ||
        !reader.u32(format) || !reader.u64(version) ||
        !reader.bytes(stored_digest.data(), stored_digest.size()) ||
        !reader.u64(payload_size)) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "invalid snapshot header",
            "snapshot.header"));
    }
    if (format != kSnapshotFormatVersion) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "unsupported snapshot format version",
            "snapshot.header.version"));
    }
    if (payload_size > reader.remaining()) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot payload is truncated",
            "snapshot.header.payload_size"));
    }
    std::string payload(static_cast<std::size_t>(payload_size), '\0');
    if (!payload.empty() && !reader.bytes(payload.data(), payload.size())) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot payload is truncated",
            "snapshot.payload"));
    }
    std::uint32_t end = 0;
    if (!reader.u32(end) || end != kSnapshotEnd || !reader.finished()) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "invalid snapshot footer",
            "snapshot.footer"));
    }
    if (digest_hex(sha256(payload)) != digest_hex(stored_digest)) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot content hash mismatch",
            "snapshot.hash"));
    }
    Outcome<RegionState> decoded = decode_region_payload(payload);
    if (!decoded) {
        return decoded;
    }
    if (decoded.value().version != version) {
        return Outcome<RegionState>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot header version mismatch",
            "snapshot.header.version"));
    }
    return decoded;
}

Outcome<std::shared_ptr<RegionStore>> RegionStore::create(RegionState initial) {
    if (initial.content_hash.empty()) {
        initial.seal();
    }
    Outcome<PublishedRegionPtr> published = build_published_region(initial);
    if (!published) {
        return Outcome<std::shared_ptr<RegionStore>>::failure(published.error());
    }
    auto store = std::shared_ptr<RegionStore>(new RegionStore());
    store->current_ = published.value();
    store->archive_[initial.version] = published.value();
    return Outcome<std::shared_ptr<RegionStore>>::success(std::move(store));
}

Outcome<std::shared_ptr<RegionStore>> RegionStore::create_archive(
    std::vector<RegionState> states) {
    if (states.empty()) {
        return Outcome<std::shared_ptr<RegionStore>>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot archive is empty",
            "snapshot.archive"));
    }
    std::sort(states.begin(), states.end(),
              [](const RegionState& lhs, const RegionState& rhs) {
                  return lhs.version < rhs.version;
              });
    for (std::size_t i = 1; i < states.size(); ++i) {
        if (states[i - 1].version == states[i].version) {
            return Outcome<std::shared_ptr<RegionStore>>::failure(region_error(
                StatusCode::SnapshotStale,
                "snapshot archive contains duplicate versions",
                "snapshot.archive.version"));
        }
    }
    auto store = std::shared_ptr<RegionStore>(new RegionStore());
    for (RegionState& state : states) {
        if (state.content_hash.empty()) {
            state.seal();
        }
        Outcome<PublishedRegionPtr> built = build_published_region(state);
        if (!built) {
            return Outcome<std::shared_ptr<RegionStore>>::failure(built.error());
        }
        store->archive_[state.version] = built.value();
        if (!store->current_ ||
            state.version > store->current_->state.version) {
            store->current_ = built.value();
        }
    }
    return Outcome<std::shared_ptr<RegionStore>>::success(std::move(store));
}

Outcome<PublishedRegionPtr> RegionStore::register_snapshot(
    PublishedRegionPtr snapshot) {
    std::lock_guard<std::mutex> lock(archive_mutex_);
    archive_[snapshot->state.version] = snapshot;
    return Outcome<PublishedRegionPtr>::success(std::move(snapshot));
}

Outcome<SnapshotHandle> RegionStore::acquire(std::uint64_t expected_version,
                                             const std::string& expected_hash) {
    PublishedRegionPtr snapshot;
    {
        std::lock_guard<std::mutex> lock(publish_mutex_);
        snapshot = current_;
    }
    if (!snapshot) {
        return Outcome<SnapshotHandle>::failure(region_error(
            StatusCode::SnapshotStale, "no published region snapshot",
            "snapshot.acquire"));
    }
    if (expected_version != 0 && snapshot->state.version != expected_version) {
        return Outcome<SnapshotHandle>::failure(region_error(
            StatusCode::RegionStale, "expected region version is not current",
            "region.version"));
    }
    if (!expected_hash.empty() && snapshot->state.content_hash != expected_hash) {
        return Outcome<SnapshotHandle>::failure(region_error(
            StatusCode::RegionStale, "expected region hash is not current",
            "region.hash"));
    }

    SnapshotHandle handle;
    handle.version = snapshot->state.version;
    handle.content_hash = snapshot->state.content_hash;
    handle.published = snapshot;
    {
        std::lock_guard<std::mutex> lock(handles_mutex_);
        handle.token = next_token_++;
        handles_[handle.token] = snapshot;
    }
    return Outcome<SnapshotHandle>::success(std::move(handle));
}

Outcome<SnapshotHandle> RegionStore::acquire_version(
    std::uint64_t version, const std::string& expected_hash) {
    PublishedRegionPtr snapshot;
    {
        std::lock_guard<std::mutex> lock(archive_mutex_);
        const auto it = archive_.find(version);
        if (it == archive_.end()) {
            return Outcome<SnapshotHandle>::failure(region_error(
                StatusCode::SnapshotStale, "requested region version is not archived",
                "snapshot.archive.version"));
        }
        snapshot = it->second;
    }
    if (!expected_hash.empty() && snapshot->state.content_hash != expected_hash) {
        return Outcome<SnapshotHandle>::failure(region_error(
            StatusCode::RegionStale, "archived region hash does not match",
            "region.hash"));
    }
    SnapshotHandle handle;
    handle.version = snapshot->state.version;
    handle.content_hash = snapshot->state.content_hash;
    handle.published = snapshot;
    {
        std::lock_guard<std::mutex> lock(handles_mutex_);
        handle.token = next_token_++;
        handles_[handle.token] = snapshot;
    }
    return Outcome<SnapshotHandle>::success(std::move(handle));
}

Outcome<PublishedRegionPtr> RegionStore::resolve(
    const SnapshotHandle& handle) const {
    if (handle.token == 0) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot handle is empty",
            "snapshot.handle"));
    }
    std::lock_guard<std::mutex> lock(handles_mutex_);
    const auto it = handles_.find(handle.token);
    if (it == handles_.end()) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot handle has been released",
            "snapshot.handle"));
    }
    return Outcome<PublishedRegionPtr>::success(it->second);
}

Outcome<void> RegionStore::release(const SnapshotHandle& handle) {
    std::lock_guard<std::mutex> lock(handles_mutex_);
    const auto it = handles_.find(handle.token);
    if (handle.token == 0 || it == handles_.end()) {
        return Outcome<void>::failure(region_error(
            StatusCode::SnapshotStale, "snapshot handle has already been released",
            "snapshot.release"));
    }
    handles_.erase(it);
    return Outcome<void>::success();
}

Outcome<PublishedRegionPtr> RegionStore::publish(RegionState next) {
    PublishedRegionPtr previous;
    {
        std::lock_guard<std::mutex> lock(publish_mutex_);
        previous = current_;
    }
    if (!previous) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::SnapshotStale, "region store has no current snapshot",
            "region.publish"));
    }
    if (next.version != previous->state.version + 1) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::RegionStale,
            "published version must be exactly one greater than current",
            "region.version"));
    }
    const std::string computed = next.compute_hash();
    if (!next.content_hash.empty() && next.content_hash != computed) {
        return Outcome<PublishedRegionPtr>::failure(region_error(
            StatusCode::RegionStale, "region hash does not match its content",
            "region.hash"));
    }
    next.content_hash = computed;
    Outcome<PublishedRegionPtr> built = build_published_region(next);
    if (!built) {
        return built;
    }

    {
        std::lock_guard<std::mutex> lock(publish_mutex_);
        if (current_->state.version + 1 != next.version) {
            return Outcome<PublishedRegionPtr>::failure(region_error(
                StatusCode::RegionStale,
                "concurrent publish changed the current version",
                "region.version"));
        }
        current_ = built.value();
    }
    return register_snapshot(built.value());
}

std::uint64_t RegionStore::current_version() const {
    std::lock_guard<std::mutex> lock(publish_mutex_);
    return current_ ? current_->state.version : 0;
}

std::string RegionStore::current_hash() const {
    std::lock_guard<std::mutex> lock(publish_mutex_);
    return current_ ? current_->state.content_hash : std::string{};
}

PublishedRegionPtr RegionStore::current_snapshot() const {
    std::lock_guard<std::mutex> lock(publish_mutex_);
    return current_;
}

RegionState RegionStore::current_state_copy() const {
    std::lock_guard<std::mutex> lock(publish_mutex_);
    return current_->state;
}

}  // namespace battle
