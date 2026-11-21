#include "terrain/version_store.h"

#include <algorithm>
#include <cstdlib>
#include <filesystem>
#include <iomanip>
#include <map>
#include <sstream>

#include "terrain/binary_io.h"
#include "terrain/hash.h"
#include "terrain/terrain_io.h"

namespace terrain {
namespace {

std::string zeroHashText() {
    return std::string(64, '0');
}

std::string versionName(std::uint64_t version, const char* suffix) {
    std::ostringstream stream;
    stream << "v" << std::setw(6) << std::setfill('0') << version << suffix;
    return stream.str();
}

std::map<std::string, std::string> parseKeyValues(const std::string& text) {
    std::map<std::string, std::string> values;
    std::istringstream stream(text);
    std::string line;
    while (std::getline(stream, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.empty() || line[0] == '#') continue;
        const std::size_t equals = line.find('=');
        if (equals == std::string::npos) continue;
        values[line.substr(0, equals)] = line.substr(equals + 1);
    }
    return values;
}

std::string bodyWithoutManifestHash(const std::string& text) {
    std::istringstream stream(text);
    std::ostringstream body;
    std::string line;
    while (std::getline(stream, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.rfind("manifest_hash=", 0) == 0) continue;
        body << line << "\n";
    }
    return body.str();
}

Status commitError(const std::string& field, const std::string& message) {
    return Status::error(E_COMMIT_FAILED, field, message);
}

bool hardCrashRequested() {
    const char* value = std::getenv("TERRAIN_FORCE_KILL");
    return value != nullptr && std::string(value) == "1";
}

}  // namespace

VersionStore::VersionStore(std::string root) : root_(std::move(root)) {}

std::string VersionStore::versionsDirectory() const {
    return joinPath(root_, "versions");
}

std::string VersionStore::manifestsDirectory() const {
    return joinPath(root_, "manifests");
}

std::string VersionStore::currentManifestPath() const {
    return joinPath(root_, "manifest.current");
}

std::string VersionStore::snapshotPath(std::uint64_t version,
                                       const char* suffix) const {
    return joinPath(versionsDirectory(), versionName(version, suffix));
}

std::string VersionStore::manifestPath(std::uint64_t version) const {
    return joinPath(manifestsDirectory(), versionName(version, ".manifest"));
}

CommitFault VersionStore::faultFromEnvironment() {
    const char* value = std::getenv("TERRAIN_COMMIT_FAULT");
    if (value == nullptr) return CommitFault::None;
    const std::string text(value);
    if (text == "before_snapshot" || text == "before_snapshot_rename") {
        return CommitFault::BeforeSnapshotRename;
    }
    if (text == "after_height" || text == "after_height_snapshot" ||
        text == "crash_after_height") {
        return CommitFault::AfterHeightSnapshot;
    }
    if (text == "before_manifest" || text == "crash_before_manifest") {
        return CommitFault::BeforeManifest;
    }
    return CommitFault::None;
}

std::string VersionStore::computeContentChain(
    const std::string& previous, const std::string& patch_hash,
    const std::string& height_hash, const std::string& material_hash,
    std::uint64_t sequence) {
    std::vector<std::uint8_t> bytes;
    bytes.insert(bytes.end(), previous.begin(), previous.end());
    bytes.push_back(static_cast<std::uint8_t>('|'));
    bytes.insert(bytes.end(), patch_hash.begin(), patch_hash.end());
    bytes.push_back(static_cast<std::uint8_t>('|'));
    bytes.insert(bytes.end(), height_hash.begin(), height_hash.end());
    bytes.push_back(static_cast<std::uint8_t>('|'));
    bytes.insert(bytes.end(), material_hash.begin(), material_hash.end());
    bytes.push_back(static_cast<std::uint8_t>('|'));
    for (int i = 0; i < 8; ++i) {
        bytes.push_back(static_cast<std::uint8_t>(
            (sequence >> (8u * i)) & 0xffu));
    }
    return toHex(sha256(bytes));
}

std::string VersionStore::serializeManifest(
    const VersionManifest& manifest) const {
    std::ostringstream body;
    body << "TERRAIN_MANIFEST 1\n";
    body << "version=" << manifest.version << "\n";
    body << "patch_sequence=" << manifest.patch_sequence << "\n";
    body << "patch_hash=" << manifest.patch_hash << "\n";
    body << "height_file=" << manifest.height_file << "\n";
    body << "height_hash=" << manifest.height_hash << "\n";
    body << "material_file=" << manifest.material_file << "\n";
    body << "material_hash=" << manifest.material_hash << "\n";
    body << "prev_manifest_hash=" << manifest.prev_manifest_hash << "\n";
    body << "content_chain=" << manifest.content_chain << "\n";
    body << "invalid_count=" << manifest.invalid_blocks.size() << "\n";
    body << "invalid_blocks=";
    for (std::size_t i = 0; i < manifest.invalid_blocks.size(); ++i) {
        if (i != 0) body << ",";
        body << manifest.invalid_blocks[i];
    }
    body << "\n";
    body << "fallback_full=" << (manifest.fallback_full ? 1 : 0) << "\n";
    const std::string body_text = body.str();
    const Digest digest = sha256(body_text.data(), body_text.size());
    std::ostringstream complete;
    complete << body_text << "manifest_hash=" << toHex(digest) << "\n";
    return complete.str();
}

Status VersionStore::parseManifest(const std::string& text,
                                   VersionManifest* manifest) const {
    std::istringstream first_line(text);
    std::string magic;
    std::uint32_t format = 0;
    first_line >> magic >> format;
    if (magic != "TERRAIN_MANIFEST" || format != 1) {
        return Status::error(E_RECOVERY_FAILED, "manifest",
                             "unsupported manifest header");
    }
    const auto values = parseKeyValues(text);
    auto require = [&](const char* key, std::string* out) -> bool {
        const auto it = values.find(key);
        if (it == values.end()) return false;
        *out = it->second;
        return true;
    };
    std::string version;
    std::string patch_sequence;
    std::string fallback;
    if (!require("version", &version) ||
        !require("patch_sequence", &patch_sequence) ||
        !require("patch_hash", &manifest->patch_hash) ||
        !require("height_file", &manifest->height_file) ||
        !require("height_hash", &manifest->height_hash) ||
        !require("material_file", &manifest->material_file) ||
        !require("material_hash", &manifest->material_hash) ||
        !require("prev_manifest_hash", &manifest->prev_manifest_hash) ||
        !require("content_chain", &manifest->content_chain) ||
        !require("fallback_full", &fallback)) {
        return Status::error(E_RECOVERY_FAILED, "manifest",
                             "manifest is missing required fields");
    }
    try {
        manifest->version = std::stoull(version);
        manifest->patch_sequence = std::stoull(patch_sequence);
        manifest->fallback_full = fallback == "1";
    } catch (...) {
        return Status::error(E_RECOVERY_FAILED, "manifest",
                             "manifest contains malformed integers");
    }
    manifest->invalid_blocks.clear();
    const auto blocks_it = values.find("invalid_blocks");
    if (blocks_it != values.end() && !blocks_it->second.empty()) {
        std::istringstream blocks(blocks_it->second);
        std::string value;
        while (std::getline(blocks, value, ',')) {
            if (value.empty()) continue;
            try {
                manifest->invalid_blocks.push_back(std::stoull(value));
            } catch (...) {
                return Status::error(E_RECOVERY_FAILED, "invalid_blocks",
                                     "manifest contains malformed block id");
            }
        }
    }
    const auto hash_it = values.find("manifest_hash");
    if (hash_it == values.end() || hash_it->second.size() != 64) {
        return Status::error(E_RECOVERY_FAILED, "manifest_hash",
                             "manifest hash is missing");
    }
    manifest->manifest_hash = hash_it->second;
    const std::string body = bodyWithoutManifestHash(text);
    const std::string actual = toHex(sha256(body.data(), body.size()));
    if (actual != manifest->manifest_hash) {
        return Status::error(E_RECOVERY_FAILED, "manifest_hash",
                             "manifest hash mismatch");
    }
    return Status::success();
}

Status VersionStore::verifyManifestFiles(const VersionManifest& manifest,
                                         HeightField* height,
                                         MaterialImage* material) const {
    const std::string height_path =
        joinPath(versionsDirectory(), manifest.height_file);
    const std::string material_path =
        joinPath(versionsDirectory(), manifest.material_file);
    HeightField loaded_height;
    MaterialImage loaded_material;
    Status status = loadHeightField(height_path, &loaded_height);
    if (!status.ok()) {
        return Status::error(E_RECOVERY_FAILED, "height_file",
                             status.toString());
    }
    status = loadMaterialImage(material_path, &loaded_material);
    if (!status.ok()) {
        return Status::error(E_RECOVERY_FAILED, "material_file",
                             status.toString());
    }
    if (computeHeightHash(loaded_height) != manifest.height_hash) {
        return Status::error(E_RECOVERY_FAILED, "height_hash",
                             "snapshot height hash mismatch");
    }
    if (computeMaterialHash(loaded_material) != manifest.material_hash) {
        return Status::error(E_RECOVERY_FAILED, "material_hash",
                             "snapshot material hash mismatch");
    }
    if (loaded_height.baseline_version != manifest.version ||
        loaded_material.baseline_version != manifest.version) {
        return Status::error(E_RECOVERY_FAILED, "version",
                             "snapshot baseline version mismatch");
    }
    if (height != nullptr) *height = std::move(loaded_height);
    if (material != nullptr) *material = std::move(loaded_material);
    return Status::success();
}

Status VersionStore::loadManifest(VersionManifest* manifest) const {
    std::vector<std::uint8_t> bytes;
    std::string error;
    if (!readFile(currentManifestPath(), &bytes, &error)) {
        return Status::error(E_RECOVERY_FAILED, "manifest.current", error);
    }
    const std::string text(bytes.begin(), bytes.end());
    return parseManifest(text, manifest);
}

Status VersionStore::loadCurrent(HeightField* height,
                                 MaterialImage* material,
                                 VersionManifest* manifest) {
    VersionManifest loaded;
    Status status = loadManifest(&loaded);
    if (!status.ok()) return status;
    status = verifyManifestFiles(loaded, height, material);
    if (!status.ok()) return status;
    current_version_ = loaded.version;
    initialized_ = true;
    if (manifest != nullptr) *manifest = std::move(loaded);
    return Status::success();
}

Status VersionStore::initializeFromFiles(const std::string& base_height,
                                         const std::string& base_material) {
    std::string error;
    if (!makeDirectories(root_, &error) ||
        !makeDirectories(versionsDirectory(), &error) ||
        !makeDirectories(manifestsDirectory(), &error)) {
        return commitError("directories", error);
    }
    if (pathExists(currentManifestPath())) {
        return loadCurrent(nullptr, nullptr, nullptr);
    }
    HeightField height;
    MaterialImage material;
    Status status = loadHeightField(base_height, &height);
    if (!status.ok()) return status;
    status = loadMaterialImage(base_material, &material);
    if (!status.ok()) return status;
    status = validateMaterialDimensions(height, material);
    if (!status.ok()) return status;
    height.baseline_version = 0;
    material.baseline_version = 0;
    std::vector<std::uint8_t> height_bytes = encodeHeightField(height);
    std::vector<std::uint8_t> material_bytes = encodeMaterialImage(material);
    const std::string height_name = versionName(0, ".height.bin");
    const std::string material_name = versionName(0, ".material.bin");
    if (!writeFileDurable(joinPath(versionsDirectory(), height_name),
                          height_bytes, &error) ||
        !writeFileDurable(joinPath(versionsDirectory(), material_name),
                          material_bytes, &error) ||
        !syncDirectory(versionsDirectory(), &error)) {
        return commitError("base_snapshot", error);
    }
    VersionManifest manifest;
    manifest.version = 0;
    manifest.patch_sequence = 0;
    manifest.patch_hash = zeroHashText();
    manifest.height_file = height_name;
    manifest.height_hash = computeHeightHash(height);
    manifest.material_file = material_name;
    manifest.material_hash = computeMaterialHash(material);
    manifest.prev_manifest_hash = zeroHashText();
    manifest.content_chain = computeContentChain(
        zeroHashText(), manifest.patch_hash, manifest.height_hash,
        manifest.material_hash, 0);
    const std::string manifest_text = serializeManifest(manifest);
    const auto values = parseKeyValues(manifest_text);
    manifest.manifest_hash = values.at("manifest_hash");
    if (!writeFileDurable(manifestPath(0), std::vector<std::uint8_t>(
                                                manifest_text.begin(),
                                                manifest_text.end()),
                          &error) ||
        !writeFileAtomic(currentManifestPath(),
                         std::vector<std::uint8_t>(manifest_text.begin(),
                                                   manifest_text.end()),
                         &error) ||
        !syncDirectory(root_, &error)) {
        return commitError("base_manifest", error);
    }
    current_version_ = 0;
    initialized_ = true;
    return Status::success();
}

Status VersionStore::commit(const HeightField& height,
                            const MaterialImage& material,
                            const CommitMetadata& metadata,
                            CommitFault fault) {
    if (!initialized_) {
        const Status loaded = loadCurrent(nullptr, nullptr, nullptr);
        if (!loaded.ok()) return loaded;
    }
    VersionManifest previous;
    Status status = loadManifest(&previous);
    if (!status.ok()) {
        return commitError("manifest.current", status.toString());
    }
    if (metadata.patch_sequence != previous.version + 1u) {
        return Status::error(E_PATCH_STALE, "patch_sequence",
                             "commit sequence does not follow manifest version");
    }
    if (height.baseline_version != metadata.patch_sequence ||
        material.baseline_version != metadata.patch_sequence) {
        return commitError("baseline_version",
                           "snapshot baseline does not match commit sequence");
    }
    status = validateMaterialDimensions(height, material);
    if (!status.ok()) return commitError("material", status.toString());

    const std::uint64_t next_version = metadata.patch_sequence;
    const std::string height_name = versionName(next_version, ".height.bin");
    const std::string material_name =
        versionName(next_version, ".material.bin");
    const std::string height_path = joinPath(versionsDirectory(), height_name);
    const std::string material_path =
        joinPath(versionsDirectory(), material_name);
    const std::string height_temp = height_path + ".tmp";
    const std::string material_temp = material_path + ".tmp";
    std::string error;
    if (fault == CommitFault::BeforeSnapshotRename) {
        return commitError("snapshot",
                           "injected failure before snapshot rename");
    }
    const std::vector<std::uint8_t> height_bytes = encodeHeightField(height);
    const std::vector<std::uint8_t> material_bytes =
        encodeMaterialImage(material);
    if (!writeFileDurable(height_temp, height_bytes, &error)) {
        removeFileIfExists(height_temp);
        return commitError("height_snapshot", error);
    }
    if (fault == CommitFault::AfterHeightSnapshot) {
        removeFileIfExists(height_temp);
        if (hardCrashRequested()) {
            std::_Exit(77);
        }
        return commitError("height_snapshot",
                           "injected termination after height snapshot");
    }
    if (!writeFileDurable(material_temp, material_bytes, &error)) {
        removeFileIfExists(height_temp);
        removeFileIfExists(material_temp);
        return commitError("material_snapshot", error);
    }
    if (!renamePath(height_temp, height_path, &error) ||
        !renamePath(material_temp, material_path, &error) ||
        !syncDirectory(versionsDirectory(), &error)) {
        removeFileIfExists(height_temp);
        removeFileIfExists(material_temp);
        return commitError("snapshot_rename", error);
    }
    if (fault == CommitFault::BeforeManifest) {
        if (hardCrashRequested()) {
            std::_Exit(77);
        }
        return commitError("manifest",
                           "injected termination after snapshots, before manifest");
    }

    VersionManifest manifest;
    manifest.version = next_version;
    manifest.patch_sequence = metadata.patch_sequence;
    manifest.patch_hash = metadata.patch_hash;
    manifest.height_file = height_name;
    manifest.height_hash = computeHeightHash(height);
    manifest.material_file = material_name;
    manifest.material_hash = computeMaterialHash(material);
    manifest.prev_manifest_hash = previous.manifest_hash;
    manifest.content_chain =
        computeContentChain(previous.content_chain, metadata.patch_hash,
                            manifest.height_hash, manifest.material_hash,
                            metadata.patch_sequence);
    manifest.invalid_blocks = metadata.invalid_blocks;
    std::sort(manifest.invalid_blocks.begin(),
              manifest.invalid_blocks.end());
    manifest.fallback_full = metadata.fallback_full;
    const std::string manifest_text = serializeManifest(manifest);
    const auto values = parseKeyValues(manifest_text);
    manifest.manifest_hash = values.at("manifest_hash");
    const std::vector<std::uint8_t> manifest_bytes(manifest_text.begin(),
                                                   manifest_text.end());
    if (!writeFileDurable(manifestPath(next_version), manifest_bytes, &error) ||
        !writeFileAtomic(currentManifestPath(), manifest_bytes, &error) ||
        !syncDirectory(root_, &error) ||
        !syncDirectory(manifestsDirectory(), &error)) {
        return commitError("manifest_commit", error);
    }
    current_version_ = next_version;
    return Status::success();
}

Status VersionStore::verifyChain() {
    VersionManifest current;
    Status current_status = loadManifest(&current);
    if (!current_status.ok()) return current_status;
    const std::uint64_t target_version = current.version;
    std::vector<std::uint64_t> versions;
    for (const std::string& path : listFiles(manifestsDirectory())) {
        if (path.size() < 13 ||
            path.substr(path.size() - 9) != ".manifest") {
            continue;
        }
        const std::string stem =
            std::filesystem::path(path).stem().string();
        if (stem.size() < 2 || stem[0] != 'v') continue;
        try {
            const std::uint64_t version = std::stoull(stem.substr(1));
            if (version <= target_version) {
                versions.push_back(version);
            }
        } catch (...) {
            return Status::error(E_RECOVERY_FAILED, "manifests",
                                 "malformed manifest filename");
        }
    }
    if (versions.empty()) {
        return Status::error(E_RECOVERY_FAILED, "manifests",
                             "no manifests found");
    }
    std::sort(versions.begin(), versions.end());
    versions.erase(std::unique(versions.begin(), versions.end()),
                   versions.end());
    std::string previous_chain = zeroHashText();
    std::string previous_manifest_hash = zeroHashText();
    for (std::size_t i = 0; i < versions.size(); ++i) {
        if (versions[i] != i) {
            return Status::error(E_RECOVERY_FAILED, "version",
                                 "manifest version sequence has a gap");
        }
        std::vector<std::uint8_t> bytes;
        std::string error;
        if (!readFile(manifestPath(versions[i]), &bytes, &error)) {
            return Status::error(E_RECOVERY_FAILED, "manifest", error);
        }
        VersionManifest manifest;
        Status status =
            parseManifest(std::string(bytes.begin(), bytes.end()), &manifest);
        if (!status.ok()) return status;
        if (manifest.version != versions[i]) {
            return Status::error(E_RECOVERY_FAILED, "version",
                                 "manifest version mismatch");
        }
        if (manifest.prev_manifest_hash != previous_manifest_hash) {
            return Status::error(E_RECOVERY_FAILED, "prev_manifest_hash",
                                 "manifest hash chain is broken");
        }
        const std::string expected_chain = computeContentChain(
            previous_chain, manifest.patch_hash, manifest.height_hash,
            manifest.material_hash, manifest.patch_sequence);
        if (manifest.content_chain != expected_chain) {
            return Status::error(E_RECOVERY_FAILED, "content_chain",
                                 "content hash chain is broken");
        }
        status = verifyManifestFiles(manifest, nullptr, nullptr);
        if (!status.ok()) return status;
        previous_chain = manifest.content_chain;
        previous_manifest_hash = manifest.manifest_hash;
    }
    if (current.version != versions.back() ||
        current.manifest_hash != previous_manifest_hash) {
        return Status::error(E_RECOVERY_FAILED, "manifest.current",
                             "current manifest does not match verified chain");
    }
    current_version_ = current.version;
    initialized_ = true;
    return Status::success();
}

}  // namespace terrain

