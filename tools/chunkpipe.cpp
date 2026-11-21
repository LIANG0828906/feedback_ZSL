#include <algorithm>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <sstream>
#include <string>
#include <vector>

#include "chunkpipe/archive.hpp"
#include "chunkpipe/compare.hpp"
#include "chunkpipe/generator.hpp"
#include "chunkpipe/hash.hpp"
#include "chunkpipe/pipeline.hpp"
#include "chunkpipe/recovery.hpp"
#include "chunkpipe/trace.hpp"
#include "chunkpipe/visibility.hpp"

namespace {

using namespace chunkpipe;

struct Arguments {
    std::vector<std::pair<std::string, std::string>> options;
    std::vector<std::string> positional;

    bool has(const std::string& name) const {
        return std::any_of(options.begin(), options.end(),
                           [&](const auto& entry) { return entry.first == name; });
    }

    std::string value(const std::string& name,
                      const std::string& fallback = {}) const {
        for (auto it = options.rbegin(); it != options.rend(); ++it) {
            if (it->first == name) {
                return it->second;
            }
        }
        return fallback;
    }

    std::vector<std::string> values(const std::string& name) const {
        std::vector<std::string> result;
        for (const auto& entry : options) {
            if (entry.first == name) {
                result.push_back(entry.second);
            }
        }
        return result;
    }

    std::uint32_t uintValue(const std::string& name,
                            std::uint32_t fallback = 0) const {
        const std::string text = value(name);
        if (text.empty()) {
            return fallback;
        }
        return static_cast<std::uint32_t>(std::stoul(text));
    }
};

Arguments parseArguments(int argc, char** argv, int start) {
    Arguments arguments;
    for (int index = start; index < argc; ++index) {
        const std::string token = argv[index];
        if (token.rfind("--", 0) == 0) {
            std::string name = token.substr(2);
            std::string value;
            const std::size_t equals = name.find('=');
            if (equals != std::string::npos) {
                value = name.substr(equals + 1);
                name = name.substr(0, equals);
            } else if (index + 1 < argc &&
                       std::string(argv[index + 1]).rfind("--", 0) != 0) {
                value = argv[++index];
            }
            arguments.options.emplace_back(std::move(name), std::move(value));
        } else {
            arguments.positional.push_back(token);
        }
    }
    return arguments;
}

Camera defaultCamera() {
    Camera camera;
    camera.position = {0.0f, 2.5f, -12.0f};
    camera.target = {0.0f, 1.5f, 7.0f};
    camera.up = {0.0f, 1.0f, 0.0f};
    camera.fovYDegrees = 60.0f;
    camera.aspect = 16.0f / 9.0f;
    camera.nearPlane = 0.1f;
    camera.farPlane = 100.0f;
    camera.width = 256;
    camera.height = 144;
    return camera;
}

std::string idsJson(const std::vector<std::uint32_t>& ids) {
    std::ostringstream output;
    output << "[";
    for (std::size_t index = 0; index < ids.size(); ++index) {
        if (index != 0) {
            output << ",";
        }
        output << ids[index];
    }
    output << "]";
    return output.str();
}

std::string statsJson(const CullStats& stats) {
    std::ostringstream output;
    output << "{\"scene_chunks\":" << stats.sceneChunks
           << ",\"loaded_chunks\":" << stats.loadedChunks
           << ",\"frustum_culled\":" << stats.frustumCulled
           << ",\"occlusion_culled\":" << stats.occlusionCulled
           << ",\"visible_chunks\":" << stats.visibleChunks
           << ",\"critical_chunks\":" << stats.criticalChunks
           << ",\"boundary_pairs\":" << stats.boundaryPairs << "}";
    return output.str();
}

void writeText(const std::filesystem::path& path, const std::string& text) {
    if (!path.parent_path().empty()) {
        std::filesystem::create_directories(path.parent_path());
    }
    std::ofstream output(path, std::ios::trunc | std::ios::binary);
    output << text;
}

void writeBusinessError(const std::filesystem::path& directory,
                        const BusinessError& error) {
    std::ostringstream output;
    output << "{\"code\":\"" << codeName(error.code) << "\",\"message\":\""
           << jsonEscape(error.message) << "\",\"locations\":[";
    for (std::size_t index = 0; index < error.locations.size(); ++index) {
        const ErrorLocation& location = error.locations[index];
        if (index != 0) {
            output << ",";
        }
        output << "{\"chunk_id\":";
        if (location.chunkId == kNoChunk) {
            output << "null";
        } else {
            output << location.chunkId;
        }
        output << ",\"related_chunk_id\":";
        if (location.relatedChunkId == kNoChunk) {
            output << "null";
        } else {
            output << location.relatedChunkId;
        }
        output << ",\"field\":\"" << jsonEscape(location.field)
               << "\",\"segment\":\"" << jsonEscape(location.segment)
               << "\",\"detail\":\"" << jsonEscape(location.detail) << "\"}";
    }
    output << "]}\n";
    writeText(directory / "error.json", output.str());
}

int exitCodeFor(Code code) {
    switch (code) {
    case Code::Ok:
        return 0;
    case Code::Busy:
        return 2;
    case Code::Cancelled:
        return 130;
    case Code::LoadFailed:
        return 5;
    default:
        return 3;
    }
}

std::vector<std::uint32_t> parseIdList(const std::string& text) {
    std::vector<std::uint32_t> ids;
    std::stringstream stream(text);
    std::string token;
    while (std::getline(stream, token, ',')) {
        if (!token.empty()) {
            ids.push_back(static_cast<std::uint32_t>(std::stoul(token)));
        }
    }
    return ids;
}

void writeRunOutputs(const std::filesystem::path& directory,
                     const std::shared_ptr<const FrameSnapshot>& snapshot) {
    if (!snapshot) {
        writeText(directory / "visibility.json",
                  "{\"published\":false,\"reason\":\"no committed snapshot\"}\n");
        writeText(directory / "stats.json", "{}\n");
        return;
    }
    std::vector<std::uint8_t> mask = snapshot->visibilityMask;
    std::ofstream maskFile(directory / "visibility.mask", std::ios::binary | std::ios::trunc);
    maskFile.write(reinterpret_cast<const char*>(mask.data()),
                   static_cast<std::streamsize>(mask.size()));
    maskFile.flush();

    std::ostringstream visibility;
    visibility << "{\"publish_version\":" << snapshot->publishVersion
               << ",\"content_hash\":\"" << hashHex(snapshot->contentHash)
               << "\",\"archive_version\":" << snapshot->archiveVersion
               << ",\"scene_id\":" << snapshot->sceneId << ",\"chunk_ids\":"
               << idsJson(snapshot->chunkIds) << ",\"loaded_chunk_ids\":"
               << idsJson(snapshot->loadedChunkIds) << ",\"mask\":\""
               << maskHex(*snapshot) << "\",\"records\":[";
    bool first = true;
    std::vector<std::uint32_t> critical;
    for (const VisibilityRecord& record : snapshot->records) {
        if (!first) {
            visibility << ",";
        }
        first = false;
        visibility << "{\"chunk_id\":" << record.chunkId
                   << ",\"flags\":" << static_cast<unsigned>(record.flags)
                   << ",\"boundary_visible_fraction\":"
                   << std::fixed << std::setprecision(6)
                   << record.boundaryVisibleFraction << "}";
        if ((record.flags & kMaskCritical) != 0) {
            critical.push_back(record.chunkId);
        }
    }
    visibility << "],\"critical_chunk_ids\":" << idsJson(critical)
               << ",\"boundary_samples\":[";
    for (std::size_t index = 0; index < snapshot->boundarySamples.size(); ++index) {
        const BoundarySample& sample = snapshot->boundarySamples[index];
        if (index != 0) {
            visibility << ",";
        }
        visibility << "{\"chunk_a\":" << sample.chunkA
                   << ",\"chunk_b\":" << sample.chunkB << ",\"segment\":\""
                   << jsonEscape(sample.segment) << "\",\"fraction_a\":"
                   << std::fixed << std::setprecision(6) << sample.fractionA
                   << ",\"fraction_b\":" << sample.fractionB
                   << ",\"consistent\":" << (sample.consistent ? "true" : "false")
                   << "}";
    }
    visibility << "]}\n";
    writeText(directory / "visibility.json", visibility.str());

    std::ostringstream stats;
    stats << "{\"publish_version\":" << snapshot->publishVersion
          << ",\"content_hash\":\"" << hashHex(snapshot->contentHash)
          << "\",\"stats\":" << statsJson(snapshot->stats) << "}\n";
    writeText(directory / "stats.json", stats.str());

    std::cout << "published_version=" << snapshot->publishVersion
              << " content_hash=" << hashHex(snapshot->contentHash) << "\n";
    std::cout << "loaded=" << snapshot->loadedChunkIds.size() << "/"
              << snapshot->stats.sceneChunks << " mask=" << maskHex(*snapshot)
              << "\n";
    std::cout << "visible=" << snapshot->stats.visibleChunks
              << " critical=" << snapshot->stats.criticalChunks
              << " frustum_culled=" << snapshot->stats.frustumCulled
              << " occlusion_culled=" << snapshot->stats.occlusionCulled << "\n";
    std::cout << "critical_chunks=";
    for (std::size_t index = 0; index < critical.size(); ++index) {
        if (index != 0) {
            std::cout << ",";
        }
        std::cout << critical[index];
    }
    std::cout << "\n";
}

SceneArchive loadSceneOrReport(const std::filesystem::path& archivePath,
                               const std::filesystem::path& outputDirectory) {
    Result<SceneArchive> scene = readArchiveFromFile(archivePath);
    if (!scene.ok()) {
        std::filesystem::create_directories(outputDirectory);
        writeBusinessError(outputDirectory, scene.error());
        std::cerr << codeName(scene.error().code) << ": " << scene.error().message
                  << "\n";
        SceneArchive empty;
        empty.version = 0;
        return empty;
    }
    return std::move(scene.value());
}

int runCommand(const Arguments& arguments, bool requireRecovery) {
    const std::filesystem::path archivePath = arguments.value("archive");
    const std::filesystem::path outputDirectory = arguments.value("out");
    if (archivePath.empty() || outputDirectory.empty()) {
        std::cerr << "--archive and --out are required\n";
        return 3;
    }
    Result<SceneArchive> read = readArchiveFromFile(archivePath);
    if (!read.ok()) {
        std::filesystem::create_directories(outputDirectory);
        writeBusinessError(outputDirectory, read.error());
        std::cerr << codeName(read.error().code) << ": " << read.error().message
                  << "\n";
        return exitCodeFor(read.error().code);
    }
    SceneArchive scene = std::move(read.value());
    std::filesystem::create_directories(outputDirectory);
    TraceLogger logger(outputDirectory / "trajectory.jsonl");

    StreamingOptions options;
    options.workerCount =
        std::max<unsigned>(1, arguments.uintValue("workers", 3));
    options.loadDelayMicros = arguments.uintValue("delay-us", 500);
    options.failChunks = parseIdList(arguments.value("fail"));
    options.cancelAfterLoads = arguments.uintValue("cancel-after");
    options.maxChunksPerRun = arguments.uintValue("max-loaded");
    options.crashAfterCommits = arguments.uintValue("crash-after");
    std::mutex progressMutex;
    options.onLoadProgress =
        [&progressMutex](std::uint32_t completed, std::uint32_t total,
                         std::uint32_t chunkId) {
            std::lock_guard<std::mutex> lock(progressMutex);
            std::cout << "[progress] loaded " << completed << "/" << total
                      << " chunk=" << chunkId << "\n";
        };

    std::optional<RecoveryStore> recovery;
    if (!arguments.has("no-recovery")) {
        recovery.emplace(outputDirectory / "recovery");
        Result<RecoveryState> recovered = recovery->recover(scene);
        if (!recovered.ok()) {
            writeBusinessError(outputDirectory, recovered.error());
            logger.event("recovery_invalid",
                         {{"code", codeName(recovered.error().code)}});
            std::cerr << codeName(recovered.error().code) << ": "
                      << recovered.error().message << "\n";
            return exitCodeFor(recovered.error().code);
        }
        if (requireRecovery && recovered.value().chunks.empty()) {
            BusinessError error;
            error.code = Code::RecoveryInvalid;
            error.message = "no committed recovery snapshots were found";
            writeBusinessError(outputDirectory, error);
            logger.event("recovery_empty");
            std::cerr << "E_RECOVERY_INVALID: no committed recovery snapshots\n";
            return 3;
        }
        logger.event("recovery_scan",
                     {{"committed", std::to_string(recovered.value().chunks.size())},
                      {"next_sequence",
                       std::to_string(recovered.value().nextSequence)},
                      {"chain", hashHex(recovered.value().chainHash)}});
        if (!recovered.value().chunks.empty()) {
            std::vector<Chunk> recoveredChunks;
            recoveredChunks.reserve(recovered.value().chunks.size());
            for (const RecoveredChunk& chunk : recovered.value().chunks) {
                recoveredChunks.push_back(chunk.chunk);
            }
            std::cout << "recovered=" << recoveredChunks.size()
                      << " committed_chunks=";
            for (std::size_t index = 0; index < recoveredChunks.size(); ++index) {
                if (index != 0) {
                    std::cout << ",";
                }
                std::cout << recoveredChunks[index].id;
            }
            std::cout << "\n";
            // Pipeline is constructed after this point; recovered data is seeded below.
            Pipeline pipeline(scene, options, &logger);
            pipeline.attachRecovery(&*recovery);
            pipeline.seedLoadedChunks(std::move(recoveredChunks));
            std::cout << "[progress] loading " << recovered.value().chunks.size()
                      << "/" << scene.chunks.size() << "\n";
            RunResult result = pipeline.tryRun(defaultCamera());
            logger.event("run_result", {{"code", codeName(result.code)}});
            writeRunOutputs(outputDirectory, result.snapshot);
            if (!result.error.message.empty() && result.code != Code::Ok) {
                writeBusinessError(outputDirectory, result.error);
                std::cerr << codeName(result.code) << ": " << result.error.message
                          << "\n";
            }
            return exitCodeFor(result.code);
        }
    }

    Pipeline pipeline(scene, options, &logger);
    if (recovery) {
        pipeline.attachRecovery(&*recovery);
    }
    std::cout << "[progress] loading 0/" << scene.chunks.size() << "\n";
    RunResult result = pipeline.tryRun(defaultCamera());
    logger.event("run_result", {{"code", codeName(result.code)}});
    writeRunOutputs(outputDirectory, result.snapshot);
    if (!result.error.message.empty() && result.code != Code::Ok) {
        writeBusinessError(outputDirectory, result.error);
        std::cerr << codeName(result.code) << ": " << result.error.message << "\n";
    }
    return exitCodeFor(result.code);
}

int generateCommand(const Arguments& arguments) {
    const std::filesystem::path outputDirectory = arguments.value("out");
    if (outputDirectory.empty()) {
        std::cerr << "--out is required\n";
        return 3;
    }
    Result<GeneratedSamples> generated =
        generateDeterministicSamples(outputDirectory);
    if (!generated.ok()) {
        writeBusinessError(outputDirectory, generated.error());
        std::cerr << codeName(generated.error().code) << ": "
                  << generated.error().message << "\n";
        return exitCodeFor(generated.error().code);
    }
    TraceLogger logger(outputDirectory / "generator_trajectory.jsonl");
    for (const auto& archive : generated.value().archives) {
        logger.event("sample_generated", {{"archive", archive.string()}});
        std::cout << "generated " << archive.string() << "\n";
    }
    std::cout << "baseline " << generated.value().baselinePath.string() << "\n";
    return 0;
}

int exportCommand(const Arguments& arguments) {
    const std::filesystem::path input = arguments.value("archive");
    const std::filesystem::path output = arguments.value("out");
    const std::uint32_t target = arguments.uintValue("to-version", 0);
    if (input.empty() || output.empty() || target < 1 || target > 3) {
        std::cerr << "--archive, --out and --to-version 1|2|3 are required\n";
        return 3;
    }
    Result<SceneArchive> scene = readArchiveFromFile(input);
    if (!scene.ok()) {
        std::cerr << codeName(scene.error().code) << ": " << scene.error().message
                  << "\n";
        return exitCodeFor(scene.error().code);
    }
    Result<std::vector<std::uint8_t>> serialized =
        serializeArchive(scene.value(), static_cast<std::uint16_t>(target));
    if (!serialized.ok()) {
        const std::filesystem::path errorDirectory =
            output.has_parent_path() ? output.parent_path() : std::filesystem::path(".");
        writeBusinessError(errorDirectory, serialized.error());
        std::cerr << codeName(serialized.error().code) << ": "
                  << serialized.error().message << "\n";
        for (const ErrorLocation& location : serialized.error().locations) {
            std::cerr << "  chunk " << location.chunkId << " field "
                      << location.field << "\n";
        }
        return exitCodeFor(serialized.error().code);
    }
    if (!output.parent_path().empty()) {
        std::filesystem::create_directories(output.parent_path());
    }
    std::ofstream file(output, std::ios::binary | std::ios::trunc);
    file.write(reinterpret_cast<const char*>(serialized.value().data()),
               static_cast<std::streamsize>(serialized.value().size()));
    file.flush();
    if (!file) {
        std::cerr << "cannot write output archive\n";
        return 3;
    }
    std::cout << "exported " << input.string() << " -> " << output.string()
              << " as v" << target << "\n";
    return 0;
}

std::vector<std::filesystem::path> compareInputs(const Arguments& arguments) {
    std::vector<std::filesystem::path> paths;
    const std::string directory = arguments.value("dir");
    if (!directory.empty()) {
        for (int version = 1; version <= 3; ++version) {
            const auto path = std::filesystem::path(directory) /
                              ("scene_v" + std::to_string(version) + ".arc");
            if (std::filesystem::exists(path)) {
                paths.push_back(path);
            }
        }
    }
    for (const std::string& archive : arguments.values("archive")) {
        paths.emplace_back(archive);
    }
    return paths;
}

int compareCommand(const Arguments& arguments) {
    const std::filesystem::path outputDirectory = arguments.value("out");
    if (outputDirectory.empty()) {
        std::cerr << "--out is required\n";
        return 3;
    }
    std::filesystem::create_directories(outputDirectory);
    TraceLogger logger(outputDirectory / "compare_trajectory.jsonl");
    const std::vector<std::filesystem::path> paths = compareInputs(arguments);
    Result<CompareReport> report = compareArchives(
        paths, defaultCamera(), arguments.uintValue("max-loaded"));
    if (!report.ok()) {
        writeBusinessError(outputDirectory, report.error());
        logger.event("compare_failed",
                     {{"code", codeName(report.error().code)}});
        std::cerr << codeName(report.error().code) << ": "
                  << report.error().message << "\n";
        return exitCodeFor(report.error().code);
    }
    writeText(outputDirectory / "diff_report.json",
              compareReportJson(report.value()));
    writeText(outputDirectory / "diff.jsonl",
              visibilityDiffJsonLines(report.value()));
    for (const VisibilityDiff& diff : report.value().differences) {
        logger.event(
            "visibility_diff",
            {{"chunk", std::to_string(diff.chunkId)},
             {"from_version", std::to_string(diff.fromVersion)},
             {"to_version", std::to_string(diff.toVersion)},
             {"from_mask", std::to_string(diff.fromMask)},
             {"to_mask", std::to_string(diff.toMask)},
             {"cause", diff.cause}});
        std::cout << "chunk " << diff.chunkId << " v" << diff.fromVersion << "->v"
                  << diff.toVersion << " mask 0x" << std::hex
                  << static_cast<unsigned>(diff.fromMask) << "->0x"
                  << static_cast<unsigned>(diff.toMask) << std::dec
                  << " cause=" << diff.cause << "\n";
    }
    std::cout << "visibility_differences=" << report.value().differences.size()
              << " metadata_differences="
              << report.value().metadataDifferences.size() << "\n";
    return 0;
}

int determinismCommand(const Arguments& arguments) {
    const std::filesystem::path archivePath = arguments.value("archive");
    const std::filesystem::path outputDirectory = arguments.value("out");
    if (archivePath.empty() || outputDirectory.empty()) {
        std::cerr << "--archive and --out are required\n";
        return 3;
    }
    std::filesystem::create_directories(outputDirectory);
    TraceLogger logger(outputDirectory / "determinism_trajectory.jsonl");
    Result<SceneArchive> read = readArchiveFromFile(archivePath);
    if (!read.ok()) {
        writeBusinessError(outputDirectory, read.error());
        return exitCodeFor(read.error().code);
    }
    StreamingOptions options;
    options.workerCount = 4;
    options.loadDelayMicros = 0;
    Pipeline pipeline(std::move(read.value()), options, &logger);
    const Camera camera = defaultCamera();
    RunResult first = pipeline.tryRun(camera);
    RunResult second = pipeline.tryRun(camera);
    const bool firstOk = first.snapshot != nullptr;
    const bool secondOk = second.snapshot != nullptr;
    const bool maskEqual =
        firstOk && secondOk &&
        first.snapshot->visibilityMask == second.snapshot->visibilityMask;
    const bool statsEqual =
        firstOk && secondOk &&
        std::memcmp(&first.snapshot->stats, &second.snapshot->stats,
                    sizeof(CullStats)) == 0;
    const bool hashEqual =
        firstOk && secondOk &&
        first.snapshot->contentHash == second.snapshot->contentHash;
    const bool deterministic = firstOk && secondOk && maskEqual && statsEqual &&
                               hashEqual;
    std::ostringstream output;
    output << "{\"deterministic\":" << (deterministic ? "true" : "false")
           << ",\"first\":{\"code\":\"" << codeName(first.code)
           << "\",\"hash\":\""
           << (firstOk ? hashHex(first.snapshot->contentHash) : "none")
           << "\",\"mask\":\"" << (firstOk ? maskHex(*first.snapshot) : "")
           << "\",\"stats\":" << (firstOk ? statsJson(first.snapshot->stats) : "{}")
           << "},\"second\":{\"code\":\"" << codeName(second.code)
           << "\",\"hash\":\""
           << (secondOk ? hashHex(second.snapshot->contentHash) : "none")
           << "\",\"mask\":\"" << (secondOk ? maskHex(*second.snapshot) : "")
           << "\",\"stats\":" << (secondOk ? statsJson(second.snapshot->stats) : "{}")
           << "}}\n";
    writeText(outputDirectory / "determinism.json", output.str());
    logger.event("determinism_check",
                 {{"result", deterministic ? "pass" : "fail"},
                  {"mask_equal", maskEqual ? "true" : "false"},
                  {"stats_equal", statsEqual ? "true" : "false"},
                  {"hash_equal", hashEqual ? "true" : "false"}});
    std::cout << "deterministic=" << (deterministic ? "true" : "false")
              << " mask_equal=" << (maskEqual ? "true" : "false")
              << " stats_equal=" << (statsEqual ? "true" : "false") << "\n";
    return deterministic ? 0 : 3;
}

void printUsage() {
    std::cout
        << "chunkpipe commands:\n"
        << "  chunkpipe generate --out DIR\n"
        << "  chunkpipe run --archive FILE --out DIR [--workers N] "
           "[--delay-us N] [--fail IDS] [--cancel-after N] [--max-loaded N] "
           "[--crash-after N] [--no-recovery]\n"
        << "  chunkpipe recover --archive FILE --out DIR [--workers N]\n"
        << "  chunkpipe compare --dir SAMPLE_DIR --out DIR [--max-loaded N]\n"
        << "  chunkpipe export --archive FILE --out FILE --to-version 1|2|3\n"
        << "  chunkpipe determinism --archive FILE --out DIR\n";
}

} // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        printUsage();
        return 3;
    }
    const std::string command = argv[1];
    try {
        const Arguments arguments = parseArguments(argc, argv, 2);
        if (command == "generate") {
            return generateCommand(arguments);
        }
        if (command == "run") {
            return runCommand(arguments, false);
        }
        if (command == "recover") {
            return runCommand(arguments, true);
        }
        if (command == "compare") {
            return compareCommand(arguments);
        }
        if (command == "export") {
            return exportCommand(arguments);
        }
        if (command == "determinism") {
            return determinismCommand(arguments);
        }
        if (command == "help" || command == "--help") {
            printUsage();
            return 0;
        }
        std::cerr << "unknown command: " << command << "\n";
        printUsage();
        return 3;
    } catch (const std::exception& exception) {
        std::cerr << "fatal: " << exception.what() << "\n";
        return 3;
    }
}

