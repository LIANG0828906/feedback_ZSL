#include "image.hpp"
#include "json.hpp"
#include "renderer.hpp"
#include "report.hpp"
#include "scene.hpp"
#include "sha256.hpp"
#include "snapshot.hpp"
#include "util.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#ifdef _WIN32
#include <windows.h>
#endif

namespace {

volatile std::sig_atomic_t g_interrupted = 0;

void handleSigint(int) { g_interrupted = 1; }

#ifdef _WIN32
BOOL WINAPI handleConsoleControl(DWORD controlType) {
    if (controlType == CTRL_C_EVENT || controlType == CTRL_BREAK_EVENT) {
        g_interrupted = 1;
        return TRUE;
    }
    return FALSE;
}
#endif

constexpr int kExitOk = 0;
constexpr int kExitValidation = 2;
constexpr int kExitDifference = 3;
constexpr int kExitRuntime = 4;

struct Options {
    std::string mode;
    std::filesystem::path scene;
    std::filesystem::path out;
    std::filesystem::path reference;
    std::filesystem::path candidate;
    std::filesystem::path snapshot;
    int frame = -1;
    int crashAfterNewCommits = -1;
    int blockDelayMs = 0;
};

void printUsage() {
    std::cout
        << "softracer 1.0.0 - deterministic offline soft-raster validator\n\n"
        << "Usage:\n"
        << "  softracer generate --out <scene.json>\n"
        << "  softracer validate --scene <scene.json> [--out <report-dir>]\n"
        << "  softracer render --scene <scene.json> --out <output-dir> [--frame N]\n"
        << "                    [--snapshot <snapshot.bin>] [--block-delay-ms N]\n"
        << "  softracer compare --reference <dir> --candidate <dir> "
           "[--out <difference.json>]\n\n"
        << "Test hooks:\n"
        << "  --crash-after-new-commits N   commit N new tiles, then call _Exit(86)\n"
        << "  --block-delay-ms N            sleep N ms after each committed tile\n";
}

bool parseInt(const std::string& text, int& value) {
    try {
        size_t consumed = 0;
        const long parsed = std::stol(text, &consumed, 10);
        if (consumed != text.size() || parsed < -2147483647L || parsed > 2147483647L) {
            return false;
        }
        value = static_cast<int>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parseOptions(int argc, char** argv, Options& options) {
    if (argc < 2) {
        printUsage();
        return false;
    }
    options.mode = argv[1];
    for (int i = 2; i < argc; ++i) {
        const std::string arg = argv[i];
        auto next = [&](std::string& value) {
            if (i + 1 >= argc) {
                std::cerr << "missing value for " << arg << "\n";
                return false;
            }
            value = argv[++i];
            return true;
        };
        if (arg == "--scene") {
            std::string value;
            if (!next(value)) return false;
            options.scene = value;
        } else if (arg == "--out") {
            std::string value;
            if (!next(value)) return false;
            options.out = value;
        } else if (arg == "--reference") {
            std::string value;
            if (!next(value)) return false;
            options.reference = value;
        } else if (arg == "--candidate") {
            std::string value;
            if (!next(value)) return false;
            options.candidate = value;
        } else if (arg == "--snapshot") {
            std::string value;
            if (!next(value)) return false;
            options.snapshot = value;
        } else if (arg == "--frame") {
            std::string value;
            if (!next(value) || !parseInt(value, options.frame)) {
                std::cerr << "invalid --frame value\n";
                return false;
            }
        } else if (arg == "--crash-after-new-commits") {
            std::string value;
            if (!next(value) || !parseInt(value, options.crashAfterNewCommits) ||
                options.crashAfterNewCommits < 0) {
                std::cerr << "invalid --crash-after-new-commits value\n";
                return false;
            }
        } else if (arg == "--block-delay-ms") {
            std::string value;
            if (!next(value) || !parseInt(value, options.blockDelayMs) ||
                options.blockDelayMs < 0) {
                std::cerr << "invalid --block-delay-ms value\n";
                return false;
            }
        } else if (arg == "--help" || arg == "-h") {
            printUsage();
            std::exit(kExitOk);
        } else {
            std::cerr << "unknown option: " << arg << "\n";
            return false;
        }
    }
    return true;
}

bool writeJsonFile(const std::filesystem::path& path, const sr::Json& json,
                   std::string& error) {
    const std::string text = json.dump(2);
    std::vector<uint8_t> bytes(text.begin(), text.end());
    return sr::writeFileBytesAtomic(path, bytes, error);
}

int reportValidationErrors(const std::vector<sr::ValidationError>& errors,
                           const std::filesystem::path& outDir,
                           const std::string& source) {
    const sr::Json report = sr::makeErrorReport(errors, source);
    std::cout << report.dump(2);
    if (!outDir.empty()) {
        std::string error;
        if (!sr::ensureDirectory(outDir, error)) {
            std::cerr << error << "\n";
        } else {
            (void)writeJsonFile(outDir / "error_report.json", report, error);
        }
    }
    return kExitValidation;
}

std::vector<uint8_t> serializeOwnership(const std::vector<uint32_t>& ownership) {
    std::vector<uint8_t> bytes;
    bytes.reserve(ownership.size() * 4U);
    for (const uint32_t value : ownership) {
        sr::appendU32(bytes, value);
    }
    return bytes;
}

bool assembleImages(const sr::Scene& scene, const sr::Renderer& renderer,
                    const std::map<int, sr::TileResult>& tiles,
                    std::vector<uint8_t>& finalRgba,
                    std::vector<uint8_t>& smokeRgba,
                    std::vector<uint32_t>& ownership) {
    const size_t pixelCount =
        static_cast<size_t>(scene.image.width) * static_cast<size_t>(scene.image.height);
    finalRgba.assign(pixelCount * 4U, 0U);
    smokeRgba.assign(pixelCount * 4U, 0U);
    ownership.assign(pixelCount, sr::kNoParticleId);
    for (int tileIndex = 0; tileIndex < renderer.tileCount(); ++tileIndex) {
        const auto it = tiles.find(tileIndex);
        if (it == tiles.end()) {
            return false;
        }
        const sr::TileResult& tile = it->second;
        for (int y = 0; y < tile.rect.height; ++y) {
            const size_t sourceOffset =
                static_cast<size_t>(y) * static_cast<size_t>(tile.rect.width);
            const size_t targetOffset =
                static_cast<size_t>(tile.rect.y + y) *
                    static_cast<size_t>(scene.image.width) +
                static_cast<size_t>(tile.rect.x);
            std::copy_n(tile.finalRgba.begin() +
                            static_cast<std::ptrdiff_t>(sourceOffset * 4U),
                        static_cast<size_t>(tile.rect.width) * 4U,
                        finalRgba.begin() +
                            static_cast<std::ptrdiff_t>(targetOffset * 4U));
            std::copy_n(tile.smokeRgba.begin() +
                            static_cast<std::ptrdiff_t>(sourceOffset * 4U),
                        static_cast<size_t>(tile.rect.width) * 4U,
                        smokeRgba.begin() +
                            static_cast<std::ptrdiff_t>(targetOffset * 4U));
            std::copy_n(tile.ownership.begin() +
                            static_cast<std::ptrdiff_t>(sourceOffset),
                        static_cast<size_t>(tile.rect.width),
                        ownership.begin() +
                            static_cast<std::ptrdiff_t>(targetOffset));
        }
    }
    return true;
}

int runGenerate(const Options& options) {
    if (options.out.empty()) {
        std::cerr << "generate requires --out\n";
        return kExitValidation;
    }
    const std::string text = sr::makeDeterministicSceneJson();
    std::vector<uint8_t> bytes(text.begin(), text.end());
    std::string error;
    if (!sr::writeFileBytesAtomic(options.out, bytes, error)) {
        std::cerr << error << "\n";
        return kExitRuntime;
    }
    std::cout << "wrote deterministic scene: " << options.out.string() << "\n";
    return kExitOk;
}

int runValidate(const Options& options) {
    if (options.scene.empty()) {
        std::cerr << "validate requires --scene\n";
        return kExitValidation;
    }
    sr::Scene scene;
    std::vector<sr::ValidationError> errors;
    if (!sr::loadScene(options.scene, scene, errors)) {
        return reportValidationErrors(errors, options.out, options.scene.string());
    }
    sr::Json report = sr::Json::object();
    report.set("status", sr::Json(std::string("ok")));
    report.set("scene_content_digest", sr::Json(sr::hexDigest(scene.digest)));
    report.set("particle_count", sr::Json(static_cast<double>(scene.particles.size())));
    report.set("light_count", sr::Json(static_cast<double>(scene.lights.size())));
    report.set("camera_sample_count",
               sr::Json(static_cast<double>(scene.cameras.size())));
    report.set("layer_count", sr::Json(static_cast<double>(scene.render.layerCount)));
    std::cout << report.dump(2);
    if (!options.out.empty()) {
        std::string error;
        (void)sr::ensureDirectory(options.out, error);
        (void)writeJsonFile(options.out / "validation_report.json", report, error);
    }
    return kExitOk;
}

int runRender(const Options& options) {
    if (options.scene.empty() || options.out.empty()) {
        std::cerr << "render requires --scene and --out\n";
        return kExitValidation;
    }
    sr::Scene scene;
    std::vector<sr::ValidationError> errors;
    if (!sr::loadScene(options.scene, scene, errors)) {
        return reportValidationErrors(errors, options.out, options.scene.string());
    }
    const int frame = sr::effectiveFrame(scene, options.frame);
    if (!sr::validateSelectedFrame(scene, frame, errors)) {
        return reportValidationErrors(errors, options.out, options.scene.string());
    }

    std::string ioError;
    if (!sr::ensureDirectory(options.out, ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }
    if (!sr::writeFileBytesAtomic(options.out / "scene.input.json", scene.rawBytes,
                                  ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }

    sr::Renderer renderer(scene, frame);
    const std::filesystem::path snapshotPath =
        options.snapshot.empty() ? options.out / "snapshot.bin" : options.snapshot;
    sr::SnapshotStore store(snapshotPath);
    std::map<int, sr::TileResult> tiles;
    sr::SnapshotLoadResult loaded = store.load(scene, frame, renderer);
    if (loaded.state == sr::SnapshotLoadState::Loaded) {
        tiles = std::move(loaded.tiles);
        std::cerr << "resumed " << tiles.size() << " committed tile(s)\n";
    } else if (loaded.state == sr::SnapshotLoadState::DiscardedCorrupt ||
               loaded.state == sr::SnapshotLoadState::DiscardedMismatch) {
        std::error_code ec;
        std::filesystem::remove(snapshotPath, ec);
        std::cerr << "discarded snapshot: " << loaded.detail << "\n";
    }

    int newCommits = 0;
    bool interrupted = false;
    for (int tileIndex = 0; tileIndex < renderer.tileCount(); ++tileIndex) {
        if (tiles.find(tileIndex) != tiles.end()) {
            continue;
        }
        if (g_interrupted != 0) {
            interrupted = true;
            break;
        }
        sr::TileResult tile = renderer.renderTile(tileIndex);
        tiles.emplace(tileIndex, std::move(tile));
        if (!store.commit(scene, frame, renderer, tiles, ioError)) {
            std::cerr << "snapshot commit failed: " << ioError << "\n";
            return kExitRuntime;
        }
        ++newCommits;
        if (options.crashAfterNewCommits >= 0 &&
            newCommits >= options.crashAfterNewCommits) {
            std::_Exit(86);
        }
        if (options.blockDelayMs > 0) {
            std::this_thread::sleep_for(
                std::chrono::milliseconds(options.blockDelayMs));
        }
    }
    if (interrupted || g_interrupted != 0) {
        std::vector<int> completed;
        for (const auto& entry : tiles) {
            completed.push_back(entry.first);
        }
        if (completed.size() < static_cast<size_t>(renderer.tileCount())) {
            const sr::Json recovery =
                sr::buildRecoveryReport(scene, renderer, completed, "SIGINT");
            if (!writeJsonFile(options.out / "recovery_report.json", recovery,
                               ioError)) {
                std::cerr << "recovery report failed: " << ioError << "\n";
                return kExitRuntime;
            }
            std::cout << "interrupted after " << completed.size()
                      << " tile(s); resume snapshot retained\n";
            return kExitOk;
        }
    }

    std::vector<uint8_t> finalRgba;
    std::vector<uint8_t> smokeRgba;
    std::vector<uint32_t> ownership;
    if (!assembleImages(scene, renderer, tiles, finalRgba, smokeRgba, ownership)) {
        std::cerr << "internal error: incomplete tile set\n";
        return kExitRuntime;
    }
    const std::vector<uint8_t> ownershipBytes = serializeOwnership(ownership);
    if (!sr::writeFileBytesAtomic(options.out / "render.rgba8", finalRgba, ioError) ||
        !sr::writeFileBytesAtomic(options.out / "smoke_premultiplied.rgba8",
                                  smokeRgba, ioError) ||
        !sr::writeFileBytesAtomic(options.out / "ownership.u32le", ownershipBytes,
                                  ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }

    sr::RgbaImage finalImage;
    finalImage.width = scene.image.width;
    finalImage.height = scene.image.height;
    finalImage.pixels = finalRgba;
    sr::RgbaImage smokeImage = finalImage;
    smokeImage.pixels = smokeRgba;
    const sr::RgbaImage ownerImage =
        sr::ownershipImage(ownership, scene.image.width, scene.image.height);
    if (!sr::writePngAtomic(options.out / "render.png", finalImage, ioError) ||
        !sr::writePngAtomic(options.out / "smoke_premultiplied.png", smokeImage,
                            ioError) ||
        !sr::writePngAtomic(options.out / "ownership.png", ownerImage, ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }
    const sr::Json landings = sr::buildShadowLandings(renderer);
    if (!writeJsonFile(options.out / "shadow_landings.json", landings, ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }

    auto hashFile = [](const std::filesystem::path& path,
                        std::array<uint8_t, 32>& digest) {
        std::vector<uint8_t> bytes;
        std::string error;
        if (!sr::readFileBytes(path, bytes, error)) {
            return false;
        }
        digest = sr::Sha256::hash(bytes);
        return true;
    };
    sr::ImageDigests digests;
    digests.finalRgba = sr::Sha256::hash(finalRgba);
    digests.smokeRgba = sr::Sha256::hash(smokeRgba);
    digests.ownership = sr::Sha256::hash(ownershipBytes);
    if (!hashFile(options.out / "render.png", digests.finalPng) ||
        !hashFile(options.out / "smoke_premultiplied.png", digests.smokePng) ||
        !hashFile(options.out / "ownership.png", digests.ownershipPng)) {
        std::cerr << "failed to hash output images\n";
        return kExitRuntime;
    }

    const sr::Json report = sr::buildStatsReport(scene, renderer, tiles, digests);
    if (!writeJsonFile(options.out / "report.json", report, ioError)) {
        std::cerr << ioError << "\n";
        return kExitRuntime;
    }
    std::cout << "rendered frame " << frame << " at t="
              << sr::formatDouble(renderer.camera().timestamp, 6) << "\n"
              << "tiles=" << renderer.tileCount()
              << " scene_sha256=" << sr::hexDigest(scene.digest) << "\n"
              << "rgba_sha256=" << sr::hexDigest(digests.finalRgba) << "\n";
    return kExitOk;
}

struct PixelDifference {
    int x = 0;
    int y = 0;
    std::vector<std::string> channels;
    std::vector<int> referenceValues;
    std::vector<int> candidateValues;
};

sr::Json jsonValueForDifference(const sr::Json& value) {
    if (value.isNumber() || value.isString() || value.isBool() || value.isNull()) {
        return value;
    }
    return sr::Json(value.dump(0));
}

bool jsonValuesEqual(const sr::Json& a, const sr::Json& b) {
    return a.dump(0) == b.dump(0);
}

void collectLandingFieldDifferences(const sr::Json& reference,
                                    const sr::Json& candidate,
                                    const std::string& prefix, sr::Json& fields) {
    if (reference.isObject() && candidate.isObject()) {
        std::set<std::string> keys;
        for (const auto& entry : reference.fields()) {
            keys.insert(entry.first);
        }
        for (const auto& entry : candidate.fields()) {
            keys.insert(entry.first);
        }
        for (const auto& key : keys) {
            const sr::Json* a = reference.get(key);
            const sr::Json* b = candidate.get(key);
            const std::string path = prefix.empty() ? key : prefix + "." + key;
            if (a == nullptr || b == nullptr) {
                sr::Json difference = sr::Json::object();
                difference.set("channel", sr::Json(path));
                difference.set("reference",
                               a != nullptr ? jsonValueForDifference(*a) : sr::Json());
                difference.set("candidate",
                               b != nullptr ? jsonValueForDifference(*b) : sr::Json());
                fields.push(std::move(difference));
            } else {
                collectLandingFieldDifferences(*a, *b, path, fields);
            }
        }
        return;
    }
    if (reference.isArray() && candidate.isArray()) {
        const size_t count =
            std::max(reference.items().size(), candidate.items().size());
        for (size_t i = 0; i < count; ++i) {
            const std::string path = prefix + "[" + std::to_string(i) + "]";
            const sr::Json* a =
                i < reference.items().size() ? &reference.items()[i] : nullptr;
            const sr::Json* b =
                i < candidate.items().size() ? &candidate.items()[i] : nullptr;
            if (a == nullptr || b == nullptr) {
                sr::Json difference = sr::Json::object();
                difference.set("channel", sr::Json(path));
                difference.set("reference",
                               a != nullptr ? jsonValueForDifference(*a) : sr::Json());
                difference.set("candidate",
                               b != nullptr ? jsonValueForDifference(*b) : sr::Json());
                fields.push(std::move(difference));
            } else {
                collectLandingFieldDifferences(*a, *b, path, fields);
            }
        }
        return;
    }
    if (!jsonValuesEqual(reference, candidate)) {
        sr::Json difference = sr::Json::object();
        difference.set("channel", sr::Json(prefix));
        difference.set("reference", jsonValueForDifference(reference));
        difference.set("candidate", jsonValueForDifference(candidate));
        fields.push(std::move(difference));
    }
}

bool readReportDimensions(const std::filesystem::path& directory, int& width,
                          int& height, int& tileWidth, int& tileHeight,
                          std::string& error) {
    std::vector<uint8_t> bytes;
    if (!sr::readFileBytes(directory / "report.json", bytes, error)) {
        return false;
    }
    sr::Json root;
    sr::JsonParseError parseError;
    const std::string text(bytes.begin(), bytes.end());
    if (!sr::parseJson(text, root, parseError)) {
        error = "cannot parse report.json in " + directory.string();
        return false;
    }
    const sr::Json* image = root.get("image");
    if (image == nullptr) {
        error = "report.json has no image object";
        return false;
    }
    const sr::Json* w = image->get("width");
    const sr::Json* h = image->get("height");
    const sr::Json* tw = image->get("tile_width");
    const sr::Json* th = image->get("tile_height");
    if (w == nullptr || h == nullptr || tw == nullptr || th == nullptr ||
        !w->isNumber() || !h->isNumber() || !tw->isNumber() || !th->isNumber()) {
        error = "report.json has invalid image dimensions";
        return false;
    }
    width = static_cast<int>(w->asNumber());
    height = static_cast<int>(h->asNumber());
    tileWidth = static_cast<int>(tw->asNumber());
    tileHeight = static_cast<int>(th->asNumber());
    return true;
}

void appendPixelChannels(std::map<int, std::vector<PixelDifference>>& blocks,
                         int tileIndex, int px, int py,
                         const std::vector<int>& ref, const std::vector<int>& cand,
                         const std::vector<std::string>& names, size_t& differenceCount) {
    PixelDifference difference;
    difference.x = px;
    difference.y = py;
    for (size_t i = 0; i < ref.size(); ++i) {
        if (ref[i] != cand[i]) {
            difference.channels.push_back(names[i]);
            difference.referenceValues.push_back(ref[i]);
            difference.candidateValues.push_back(cand[i]);
            ++differenceCount;
        }
    }
    if (!difference.channels.empty()) {
        blocks[tileIndex].push_back(std::move(difference));
    }
}

int runCompare(const Options& options) {
    if (options.reference.empty() || options.candidate.empty()) {
        std::cerr << "compare requires --reference and --candidate\n";
        return kExitValidation;
    }
    int referenceWidth = 0;
    int referenceHeight = 0;
    int referenceTileWidth = 0;
    int referenceTileHeight = 0;
    int candidateWidth = 0;
    int candidateHeight = 0;
    int candidateTileWidth = 0;
    int candidateTileHeight = 0;
    std::string error;
    if (!readReportDimensions(options.reference, referenceWidth, referenceHeight,
                              referenceTileWidth, referenceTileHeight, error) ||
        !readReportDimensions(options.candidate, candidateWidth, candidateHeight,
                              candidateTileWidth, candidateTileHeight, error)) {
        std::cerr << error << "\n";
        return kExitValidation;
    }
    if (referenceWidth != candidateWidth || referenceHeight != candidateHeight ||
        referenceTileWidth != candidateTileWidth ||
        referenceTileHeight != candidateTileHeight) {
        std::cerr << "reference and candidate image/tile dimensions differ\n";
        sr::Json report = sr::Json::object();
        report.set("status", sr::Json(std::string("difference")));
        report.set("reference", sr::Json(options.reference.string()));
        report.set("candidate", sr::Json(options.candidate.string()));
        sr::Json summary = sr::Json::object();
        summary.set("reason", sr::Json(std::string("dimension_mismatch")));
        report.set("summary", std::move(summary));
        sr::Json dimensions = sr::Json::object();
        dimensions.set("reference_width", sr::Json(static_cast<double>(referenceWidth)));
        dimensions.set("reference_height",
                       sr::Json(static_cast<double>(referenceHeight)));
        dimensions.set("reference_tile_width",
                       sr::Json(static_cast<double>(referenceTileWidth)));
        dimensions.set("reference_tile_height",
                       sr::Json(static_cast<double>(referenceTileHeight)));
        dimensions.set("candidate_width", sr::Json(static_cast<double>(candidateWidth)));
        dimensions.set("candidate_height",
                       sr::Json(static_cast<double>(candidateHeight)));
        dimensions.set("candidate_tile_width",
                       sr::Json(static_cast<double>(candidateTileWidth)));
        dimensions.set("candidate_tile_height",
                       sr::Json(static_cast<double>(candidateTileHeight)));
        report.set("dimensions", std::move(dimensions));
        report.set("blocks", sr::Json::array());
        const std::filesystem::path diffPath =
            options.out.empty() ? options.candidate / "difference_report.json"
                                : options.out;
        if (!writeJsonFile(diffPath, report, error)) {
            std::cerr << error << "\n";
            return kExitRuntime;
        }
        return kExitDifference;
    }
    const int width = referenceWidth;
    const int height = referenceHeight;
    const int tileWidth = referenceTileWidth;
    const int tileHeight = referenceTileHeight;
    if (width <= 0 || height <= 0 || tileWidth <= 0 || tileHeight <= 0) {
        std::cerr << "invalid image dimensions in report\n";
        return kExitValidation;
    }

    std::vector<uint8_t> referenceFinal;
    std::vector<uint8_t> candidateFinal;
    if (!sr::readFileBytes(options.reference / "render.rgba8", referenceFinal, error) ||
        !sr::readFileBytes(options.candidate / "render.rgba8", candidateFinal, error)) {
        std::cerr << error << "\n";
        return kExitValidation;
    }
    std::vector<uint8_t> referenceOwner;
    std::vector<uint8_t> candidateOwner;
    if (!sr::readFileBytes(options.reference / "ownership.u32le", referenceOwner,
                           error) ||
        !sr::readFileBytes(options.candidate / "ownership.u32le", candidateOwner,
                           error)) {
        std::cerr << error << "\n";
        return kExitValidation;
    }
    const size_t expectedFinal =
        static_cast<size_t>(width) * static_cast<size_t>(height) * 4U;
    const size_t expectedOwner =
        static_cast<size_t>(width) * static_cast<size_t>(height) * 4U;
    if (referenceFinal.size() != expectedFinal ||
        candidateFinal.size() != expectedFinal ||
        referenceOwner.size() != expectedOwner ||
        candidateOwner.size() != expectedOwner) {
        std::cerr << "input image dimensions do not match report metadata\n";
        return kExitValidation;
    }

    std::map<int, std::vector<PixelDifference>> blocks;
    size_t differenceCount = 0;
    const std::vector<std::string> rgbaNames = {"R", "G", "B", "A"};
    const std::vector<std::string> ownerNames = {"ID_B0", "ID_B1", "ID_B2", "ID_B3"};
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const size_t pixel = static_cast<size_t>(y) * static_cast<size_t>(width) +
                                 static_cast<size_t>(x);
            std::vector<int> refFinal(4);
            std::vector<int> candFinal(4);
            std::vector<int> refOwner(4);
            std::vector<int> candOwner(4);
            for (int c = 0; c < 4; ++c) {
                refFinal[static_cast<size_t>(c)] = referenceFinal[pixel * 4U + c];
                candFinal[static_cast<size_t>(c)] = candidateFinal[pixel * 4U + c];
                refOwner[static_cast<size_t>(c)] = referenceOwner[pixel * 4U + c];
                candOwner[static_cast<size_t>(c)] = candidateOwner[pixel * 4U + c];
            }
            const int tileIndex = (y / tileHeight) * ((width + tileWidth - 1) / tileWidth) +
                                  (x / tileWidth);
            appendPixelChannels(blocks, tileIndex, x, y, refFinal, candFinal, rgbaNames,
                                differenceCount);
            appendPixelChannels(blocks, tileIndex, x, y, refOwner, candOwner,
                                ownerNames, differenceCount);
        }
    }

    std::vector<uint8_t> referenceLandings;
    std::vector<uint8_t> candidateLandings;
    const bool landingsRead =
        sr::readFileBytes(options.reference / "shadow_landings.json",
                          referenceLandings, error) &&
        sr::readFileBytes(options.candidate / "shadow_landings.json",
                          candidateLandings, error);
    const bool landingsMatch = landingsRead && referenceLandings == candidateLandings;
    sr::Json landingDifferences = sr::Json::array();
    if (!landingsMatch) {
        sr::Json referenceJson;
        sr::Json candidateJson;
        sr::JsonParseError referenceParseError;
        sr::JsonParseError candidateParseError;
        const std::string referenceText(referenceLandings.begin(),
                                        referenceLandings.end());
        const std::string candidateText(candidateLandings.begin(),
                                        candidateLandings.end());
        if (landingsRead &&
            sr::parseJson(referenceText, referenceJson, referenceParseError) &&
            sr::parseJson(candidateText, candidateJson, candidateParseError)) {
            const sr::Json* referenceItems = referenceJson.get("landings");
            const sr::Json* candidateItems = candidateJson.get("landings");
            const size_t count =
                std::max(referenceItems != nullptr && referenceItems->isArray()
                             ? referenceItems->items().size()
                             : 0U,
                         candidateItems != nullptr && candidateItems->isArray()
                             ? candidateItems->items().size()
                             : 0U);
            for (size_t i = 0; i < count; ++i) {
                const sr::Json* a =
                    referenceItems != nullptr && referenceItems->isArray() &&
                            i < referenceItems->items().size()
                        ? &referenceItems->items()[i]
                        : nullptr;
                const sr::Json* b =
                    candidateItems != nullptr && candidateItems->isArray() &&
                            i < candidateItems->items().size()
                        ? &candidateItems->items()[i]
                        : nullptr;
                sr::Json item = sr::Json::object();
                item.set("landing_index", sr::Json(static_cast<double>(i)));
                if (a != nullptr && a->isObject()) {
                    if (const sr::Json* id = a->get("particle_id")) {
                        item.set("particle_id", *id);
                    }
                    if (const sr::Json* light = a->get("light_id")) {
                        item.set("light_id", *light);
                    }
                }
                sr::Json fields = sr::Json::array();
                if (a == nullptr || b == nullptr) {
                    sr::Json missing = sr::Json::object();
                    missing.set("channel", sr::Json(std::string("landing")));
                    missing.set("reference",
                                a != nullptr ? jsonValueForDifference(*a) : sr::Json());
                    missing.set("candidate",
                                b != nullptr ? jsonValueForDifference(*b) : sr::Json());
                    fields.push(std::move(missing));
                } else {
                    collectLandingFieldDifferences(*a, *b, "", fields);
                }
                if (!fields.items().empty()) {
                    item.set("fields", std::move(fields));
                    landingDifferences.push(std::move(item));
                }
            }
        } else if (landingsRead) {
            sr::Json item = sr::Json::object();
            item.set("landing_index", sr::Json(-1.0));
            item.set("parse_error",
                     sr::Json(std::string("shadow_landings.json is not valid JSON")));
            landingDifferences.push(std::move(item));
        }
    }

    const bool match = differenceCount == 0 && landingsMatch;
    sr::Json report = sr::Json::object();
    report.set("status", sr::Json(std::string(match ? "match" : "difference")));
    report.set("reference", sr::Json(options.reference.string()));
    report.set("candidate", sr::Json(options.candidate.string()));
    sr::Json summary = sr::Json::object();
    summary.set("differing_channels",
                sr::Json(static_cast<double>(differenceCount)));
    summary.set("shadow_landings_match", sr::Json(landingsMatch));
    summary.set("shadow_landing_differences",
                sr::Json(static_cast<double>(landingDifferences.items().size())));
    report.set("summary", std::move(summary));
    sr::Json blockArray = sr::Json::array();
    size_t emitted = 0;
    for (const auto& entry : blocks) {
        sr::Json block = sr::Json::object();
        const int blockX =
            (entry.first % ((width + tileWidth - 1) / tileWidth)) * tileWidth;
        const int blockY =
            (entry.first / ((width + tileWidth - 1) / tileWidth)) * tileHeight;
        block.set("tile", sr::Json(static_cast<double>(entry.first)));
        block.set("block_x", sr::Json(static_cast<double>(blockX)));
        block.set("block_y", sr::Json(static_cast<double>(blockY)));
        sr::Json pixels = sr::Json::array();
        for (const auto& difference : entry.second) {
            if (emitted >= 10000U) {
                continue;
            }
            sr::Json pixel = sr::Json::object();
            pixel.set("x", sr::Json(static_cast<double>(difference.x)));
            pixel.set("y", sr::Json(static_cast<double>(difference.y)));
            sr::Json channels = sr::Json::array();
            sr::Json referenceValues = sr::Json::array();
            sr::Json candidateValues = sr::Json::array();
            for (size_t i = 0; i < difference.channels.size(); ++i) {
                channels.push(sr::Json(difference.channels[i]));
                referenceValues.push(
                    sr::Json(static_cast<double>(difference.referenceValues[i])));
                candidateValues.push(
                    sr::Json(static_cast<double>(difference.candidateValues[i])));
            }
            pixel.set("channels", std::move(channels));
            pixel.set("reference_values", std::move(referenceValues));
            pixel.set("candidate_values", std::move(candidateValues));
            pixels.push(std::move(pixel));
            ++emitted;
        }
        block.set("pixels", std::move(pixels));
        blockArray.push(std::move(block));
    }
    report.set("blocks", std::move(blockArray));
    report.set("shadow_landing_differences", std::move(landingDifferences));
    report.set("differences_truncated", sr::Json(emitted < differenceCount));

    const std::filesystem::path diffPath =
        options.out.empty() ? options.candidate / "difference_report.json" : options.out;
    if (!writeJsonFile(diffPath, report, error)) {
        std::cerr << error << "\n";
        return kExitRuntime;
    }
    std::cout << (match ? "images match byte-for-byte\n"
                        : "differences found; see " + diffPath.string() + "\n");
    return match ? kExitOk : kExitDifference;
}

}  // namespace

int main(int argc, char** argv) {
    std::signal(SIGINT, handleSigint);
#ifdef _WIN32
    SetConsoleCtrlHandler(handleConsoleControl, TRUE);
#endif
    if (argc == 2 &&
        (std::string(argv[1]) == "--help" || std::string(argv[1]) == "-h")) {
        printUsage();
        return kExitOk;
    }
    Options options;
    if (!parseOptions(argc, argv, options)) {
        return kExitValidation;
    }
    if (options.mode == "generate") {
        return runGenerate(options);
    }
    if (options.mode == "validate") {
        return runValidate(options);
    }
    if (options.mode == "render") {
        return runRender(options);
    }
    if (options.mode == "compare") {
        return runCompare(options);
    }
    std::cerr << "unknown mode: " << options.mode << "\n";
    printUsage();
    return kExitValidation;
}
