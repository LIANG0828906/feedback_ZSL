#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <set>
#include <string>
#include <thread>
#include <vector>

#include "chunkpipe/archive.hpp"
#include "chunkpipe/compare.hpp"
#include "chunkpipe/generator.hpp"
#include "chunkpipe/hash.hpp"
#include "chunkpipe/pipeline.hpp"
#include "chunkpipe/recovery.hpp"
#include "chunkpipe/visibility.hpp"

namespace {

using namespace chunkpipe;

struct TestContext {
    int checks = 0;
    int failures = 0;

    bool check(bool condition,
               const std::string& expression,
               const std::string& file,
               int line) {
        ++checks;
        if (!condition) {
            ++failures;
            std::cerr << "FAIL " << file << ":" << line << " " << expression
                      << "\n";
        }
        return condition;
    }
};

#define CHECK(context, expression) \
    (context).check((expression), #expression, __FILE__, __LINE__)

std::vector<std::uint8_t> readBytes(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    input.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (size > 0) {
        input.read(reinterpret_cast<char*>(bytes.data()), size);
    }
    return bytes;
}

void writeBytes(const std::filesystem::path& path,
                const std::vector<std::uint8_t>& bytes) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    output.write(reinterpret_cast<const char*>(bytes.data()),
                 static_cast<std::streamsize>(bytes.size()));
}

template <typename T>
void expectFailure(TestContext& context,
                   const Result<T>& result,
                   Code code,
                   const std::string& label,
                   const std::string& expectedField = {}) {
    if (!CHECK(context, !result.ok())) {
        return;
    }
    CHECK(context, result.error().code == code);
    CHECK(context, !result.error().locations.empty());
    if (!expectedField.empty()) {
        bool foundField = false;
        for (const ErrorLocation& location : result.error().locations) {
            if (location.field == expectedField) {
                foundField = true;
                break;
            }
        }
        if (!CHECK(context, foundField)) {
            std::cerr << "  missing field " << expectedField << " for " << label
                      << "\n";
        }
    }
}

bool errorHasField(const BusinessError& error, const std::string& field) {
    return std::any_of(error.locations.begin(), error.locations.end(),
                       [&](const ErrorLocation& location) {
                           return location.field == field;
                       });
}

void testGeneratedArchives(TestContext& context,
                           const GeneratedSamples& generated) {
    CHECK(context, generated.archives.size() == 3);
    CHECK(context, std::filesystem::exists(generated.baselinePath));

    Result<SceneArchive> v1 = readArchiveFromFile(generated.archives[0]);
    Result<SceneArchive> v2 = readArchiveFromFile(generated.archives[1]);
    Result<SceneArchive> v3 = readArchiveFromFile(generated.archives[2]);
    CHECK(context, v1.ok() && v2.ok() && v3.ok());
    if (!v1.ok() || !v2.ok() || !v3.ok()) {
        return;
    }
    CHECK(context, v1.value().version == 1);
    CHECK(context, v2.value().version == 2);
    CHECK(context, v3.value().version == 3);
    CHECK(context, v1.value().chunks.size() == 9);
    CHECK(context, v2.value().chunks.size() == 9);
    CHECK(context, v3.value().chunks.size() == 9);

    for (const Chunk& chunk : v1.value().chunks) {
        CHECK(context, !chunk.grid.has_value());
        CHECK(context, !chunk.occluder.has_value());
        CHECK(context, !chunk.compression.has_value());
        CHECK(context, !chunk.hierarchy.has_value());
        CHECK(context, chunk.prioritySource == PrioritySource::DerivedFromBounds);
        CHECK(context, chunk.priority == derivePriority(chunk.bounds));
    }
    for (const Chunk& chunk : v2.value().chunks) {
        CHECK(context, chunk.grid.has_value());
        CHECK(context, chunk.occluder.has_value());
        CHECK(context, !chunk.compression.has_value());
        CHECK(context, !chunk.hierarchy.has_value());
        CHECK(context, chunk.prioritySource == PrioritySource::DerivedFromBounds);
    }
    for (const Chunk& chunk : v3.value().chunks) {
        CHECK(context, chunk.grid.has_value());
        CHECK(context, chunk.occluder.has_value());
        CHECK(context, chunk.compression.has_value());
        CHECK(context, chunk.hierarchy.has_value());
        CHECK(context, chunk.prioritySource == PrioritySource::AuthoredV3);
    }
    const auto root = std::find_if(v3.value().chunks.begin(), v3.value().chunks.end(),
                                   [](const Chunk& chunk) { return chunk.id == 4; });
    CHECK(context, root != v3.value().chunks.end());
    if (root != v3.value().chunks.end() && root->hierarchy) {
        CHECK(context, root->hierarchy->children.size() == 4);
    }
}

void testParseFailures(TestContext& context,
                       const GeneratedSamples& generated,
                       const std::filesystem::path& tempDirectory) {
    const std::vector<std::uint8_t> validV1 = readBytes(generated.archives[0]);
    CHECK(context, validV1.size() > 200);

    std::vector<std::uint8_t> unknownVersion = validV1;
    unknownVersion[8] = 9;
    Result<SceneArchive> unknown = readArchiveFromBytes(unknownVersion);
    expectFailure(context, unknown, Code::ChunkInvalid, "unknown version",
                  "version");

    std::vector<std::uint8_t> truncated = validV1;
    truncated.resize(truncated.size() - 13);
    Result<SceneArchive> truncatedResult = readArchiveFromBytes(truncated);
    expectFailure(context, truncatedResult, Code::ChunkInvalid, "truncated");
    if (!truncatedResult.ok() && !truncatedResult.error().locations.empty()) {
        CHECK(context, !truncatedResult.error().locations.front().field.empty());
    }

    std::vector<std::uint8_t> badIndex = validV1;
    const std::size_t firstIndexOffset = 32 + 4 + 24 + 4 + 4 + 8 * 12;
    const std::uint32_t invalidIndex = 999;
    std::memcpy(badIndex.data() + firstIndexOffset, &invalidIndex,
                sizeof(invalidIndex));
    Result<SceneArchive> badIndexResult = readArchiveFromBytes(badIndex);
    expectFailure(context, badIndexResult, Code::ChunkInvalid, "bad index",
                  "indices");
    if (!badIndexResult.ok() && !badIndexResult.error().locations.empty()) {
        CHECK(context, badIndexResult.error().locations.front().chunkId == 0);
    }

    std::vector<std::uint8_t> badBounds = validV1;
    const float outside = 100.0f;
    const std::size_t firstVertexOffset = 32 + 4 + 24 + 4 + 4;
    std::memcpy(badBounds.data() + firstVertexOffset, &outside, sizeof(outside));
    Result<SceneArchive> badBoundsResult = readArchiveFromBytes(badBounds);
    expectFailure(context, badBoundsResult, Code::ChunkInvalid, "bad bounds",
                  "bounds");

    const auto badFile = tempDirectory / "bad-index.arc";
    writeBytes(badFile, badIndex);
    Result<SceneArchive> badFileResult = readArchiveFromFile(badFile);
    expectFailure(context, badFileResult, Code::ChunkInvalid, "bad file",
                  "indices");
}

void testUpgradeAndVersionLoss(TestContext& context,
                               const GeneratedSamples& generated) {
    Result<SceneArchive> v1 = readArchiveFromFile(generated.archives[0]);
    Result<SceneArchive> v2 = readArchiveFromFile(generated.archives[1]);
    Result<SceneArchive> v3 = readArchiveFromFile(generated.archives[2]);
    CHECK(context, v1.ok() && v2.ok() && v3.ok());
    if (!v1.ok() || !v2.ok() || !v3.ok()) {
        return;
    }

    Result<std::vector<std::uint8_t>> v1ToV3 = serializeArchive(v1.value(), 3);
    CHECK(context, v1ToV3.ok());
    if (v1ToV3.ok()) {
        Result<SceneArchive> upgraded = readArchiveFromBytes(v1ToV3.value());
        CHECK(context, upgraded.ok());
        if (upgraded.ok()) {
            CHECK(context, upgraded.value().version == 3);
            for (const Chunk& chunk : upgraded.value().chunks) {
                CHECK(context,
                      chunk.prioritySource == PrioritySource::DerivedFromBounds);
                CHECK(context, !chunk.occluder.has_value());
                CHECK(context, !chunk.hierarchy.has_value());
            }
        }
    }

    Result<std::vector<std::uint8_t>> v2ToV3 = serializeArchive(v2.value(), 3);
    CHECK(context, v2ToV3.ok());
    if (v2ToV3.ok()) {
        Result<SceneArchive> upgraded = readArchiveFromBytes(v2ToV3.value());
        CHECK(context, upgraded.ok());
        if (upgraded.ok()) {
            CHECK(context, upgraded.value().chunks.front().occluder.has_value());
            CHECK(context,
                  upgraded.value().chunks.front().prioritySource ==
                      PrioritySource::DerivedFromBounds);
            CHECK(context, !upgraded.value().chunks.front().compression.has_value());
        }
    }

    Result<void> v1ToV1 = checkVersionLoss(v1.value(), 1);
    CHECK(context, v1ToV1.ok());
    Result<std::vector<std::uint8_t>> v1Export = serializeArchive(v1.value(), 1);
    CHECK(context, v1Export.ok());

    Result<std::vector<std::uint8_t>> v3ToV2 = serializeArchive(v3.value(), 2);
    expectFailure(context, v3ToV2, Code::VersionLoss, "v3 to v2", "compression");
    if (!v3ToV2.ok()) {
        CHECK(context, errorHasField(v3ToV2.error(), "priority"));
        CHECK(context, errorHasField(v3ToV2.error(), "hierarchy"));
        CHECK(context, v3ToV2.error().locations.size() > 9);
    }

    Result<std::vector<std::uint8_t>> v2ToV1 = serializeArchive(v2.value(), 1);
    expectFailure(context, v2ToV1, Code::VersionLoss, "v2 to v1", "grid");
    if (!v2ToV1.ok()) {
        CHECK(context, errorHasField(v2ToV1.error(), "occluder"));
    }
}

void testReferenceValidation(TestContext& context,
                             const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }

    SceneArchive dangling = read.value();
    dangling.chunks.front().hierarchy->children.push_back(999);
    Result<void> danglingResult = validateScene(dangling);
    expectFailure(context, danglingResult, Code::ChunkInvalid,
                  "dangling hierarchy", "hierarchy.children");

    SceneArchive invalidIndex = read.value();
    invalidIndex.chunks.front().indices.front() = 1234;
    Result<void> invalidIndexResult = validateScene(invalidIndex);
    expectFailure(context, invalidIndexResult, Code::ChunkInvalid,
                  "invalid index", "indices");

    SceneArchive duplicateId = read.value();
    duplicateId.chunks[1].id = duplicateId.chunks[0].id;
    Result<void> duplicateResult = validateScene(duplicateId);
    expectFailure(context, duplicateResult, Code::ChunkInvalid, "duplicate id",
                  "id");

    SceneArchive emptyOccluder = read.value();
    emptyOccluder.chunks.front().vertices.clear();
    emptyOccluder.chunks.front().indices.clear();
    emptyOccluder.chunks.front().occluder = true;
    Result<void> emptyOccluderResult = validateScene(emptyOccluder);
    expectFailure(context, emptyOccluderResult, Code::ChunkInvalid,
                  "empty occluder", "occluder");
}

void testPipelineAndPublishing(TestContext& context,
                               const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    StreamingOptions options;
    options.workerCount = 4;
    options.loadDelayMicros = 500;
    Pipeline pipeline(read.value(), options);
    RunResult first = pipeline.tryRun(generated.camera);
    CHECK(context, first.code == Code::Ok);
    CHECK(context, first.scheduled.size() == 9);
    CHECK(context, first.snapshot != nullptr);
    if (!first.snapshot) {
        return;
    }
    CHECK(context, first.snapshot->loadedChunkIds.size() == 9);
    CHECK(context, first.snapshot->stats.sceneChunks == 9);
    CHECK(context, first.snapshot->stats.occlusionCulled == 6);
    CHECK(context, first.snapshot->publishVersion == 1);
    const std::uint32_t firstVersion = first.snapshot->publishVersion;
    const std::uint64_t firstHash = first.snapshot->contentHash;

    Result<std::shared_ptr<const FrameSnapshot>> current =
        pipeline.queryPublished(firstVersion, firstHash);
    CHECK(context, current.ok());
    Result<std::shared_ptr<const FrameSnapshot>> stale =
        pipeline.queryPublished(firstVersion + 1, firstHash);
    expectFailure(context, stale, Code::VersionStale, "stale version");
    Result<std::shared_ptr<const FrameSnapshot>> staleHash =
        pipeline.queryPublished(firstVersion, firstHash ^ 0x1ull);
    expectFailure(context, staleHash, Code::VersionStale, "stale hash");

    RunResult second = pipeline.tryRun(generated.camera);
    CHECK(context, second.code == Code::Ok);
    CHECK(context, second.scheduled.empty());
    CHECK(context, second.snapshot != nullptr);
    if (second.snapshot) {
        CHECK(context, second.snapshot->publishVersion == 2);
        CHECK(context, second.snapshot->contentHash == firstHash);
        CHECK(context, second.snapshot->loadedChunkIds.size() == 9);
    }

    StreamingOptions failOptions;
    failOptions.workerCount = 2;
    failOptions.loadDelayMicros = 0;
    failOptions.failChunks = {4};
    Pipeline failPipeline(read.value(), failOptions);
    RunResult failed = failPipeline.tryRun(generated.camera);
    CHECK(context, failed.code == Code::LoadFailed);
    CHECK(context, failed.loadFailures.size() == 1);
    CHECK(context, failed.snapshot != nullptr);
    if (failed.snapshot) {
        CHECK(context, failed.snapshot->loadedChunkIds.size() == 8);
        CHECK(context, failed.snapshot->visibilityMask[4] == 0);
        bool recordPresent = false;
        for (const VisibilityRecord& record : failed.snapshot->records) {
            if (record.chunkId == 4) {
                recordPresent = true;
            }
        }
        CHECK(context, !recordPresent);
    }
    if (!failed.loadFailures.empty()) {
        CHECK(context, failed.loadFailures.front().code == Code::LoadFailed);
        CHECK(context, !failed.loadFailures.front().locations.empty());
        CHECK(context, failed.loadFailures.front().locations.front().chunkId == 4);
        CHECK(context, failed.loadFailures.front().locations.front().field == "load");
    }
}

void testCancellationPreservesSnapshot(TestContext& context,
                                       const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    StreamingOptions options;
    options.workerCount = 1;
    options.loadDelayMicros = 0;
    options.maxChunksPerRun = 3;
    options.cancelAfterLoads = 0;
    Pipeline pipeline(read.value(), options);
    RunResult initial = pipeline.tryRun(generated.camera);
    CHECK(context, initial.code == Code::Ok);
    CHECK(context, initial.snapshot != nullptr);
    if (!initial.snapshot) {
        return;
    }
    CHECK(context, initial.snapshot->loadedChunkIds.size() == 3);
    const std::uint32_t version = initial.snapshot->publishVersion;
    const std::uint64_t hash = initial.snapshot->contentHash;
    const std::shared_ptr<const FrameSnapshot> before = initial.snapshot;

    pipeline.setCancelAfterLoads(1);
    RunResult cancelled = pipeline.tryRun(generated.camera);
    CHECK(context, cancelled.code == Code::Cancelled);
    CHECK(context, cancelled.cancelled);
    CHECK(context, cancelled.snapshot == before);
    CHECK(context, cancelled.snapshot->publishVersion == version);
    CHECK(context, cancelled.snapshot->contentHash == hash);
    CHECK(context, cancelled.snapshot->loadedChunkIds.size() == 3);
}

void testBusyBoundary(TestContext& context,
                      const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    StreamingOptions options;
    options.workerCount = 1;
    options.loadDelayMicros = 40000;
    Pipeline pipeline(read.value(), options);
    Pipeline otherPipeline(read.value(), options);
    RunResult first;
    std::thread worker([&]() { first = pipeline.tryRun(generated.camera); });
    std::this_thread::sleep_for(std::chrono::milliseconds(25));
    RunResult busy = pipeline.tryRun(generated.camera);
    CHECK(context, busy.code == Code::Busy);
    RunResult crossInstanceBusy = otherPipeline.tryRun(generated.camera);
    CHECK(context, crossInstanceBusy.code == Code::Busy);
    worker.join();
    CHECK(context, first.code == Code::Ok);
    CHECK(context, first.snapshot != nullptr);
}

void testDeterminism(TestContext& context,
                     const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    StreamingOptions options;
    options.workerCount = 4;
    options.loadDelayMicros = 0;
    Pipeline firstPipeline(read.value(), options);
    Pipeline secondPipeline(read.value(), options);
    RunResult first = firstPipeline.tryRun(generated.camera);
    RunResult second = secondPipeline.tryRun(generated.camera);
    CHECK(context, first.code == Code::Ok);
    CHECK(context, second.code == Code::Ok);
    CHECK(context, first.snapshot != nullptr && second.snapshot != nullptr);
    if (!first.snapshot || !second.snapshot) {
        return;
    }
    CHECK(context,
          first.snapshot->visibilityMask == second.snapshot->visibilityMask);
    CHECK(context,
          std::memcmp(&first.snapshot->stats, &second.snapshot->stats,
                      sizeof(CullStats)) == 0);
    CHECK(context, first.snapshot->contentHash == second.snapshot->contentHash);
    CHECK(context, maskHex(*first.snapshot) == maskHex(*second.snapshot));
}

void testBoundaryMismatch(TestContext& context) {
    BoundarySample sample;
    sample.chunkA = 5;
    sample.chunkB = 6;
    sample.segment = "x=3.0000 y=[0,4] z=[2,5]";
    sample.fractionA = 1.0f;
    sample.fractionB = 0.2f;
    sample.consistent = false;
    Result<void> mismatch = checkBoundaryConsistency({sample}, 0.1f);
    expectFailure(context, mismatch, Code::VisibilityMismatch,
                  "boundary mismatch", "visibility.boundary");
    if (!mismatch.ok()) {
        CHECK(context, mismatch.error().locations.front().chunkId == 5);
        CHECK(context, mismatch.error().locations.front().relatedChunkId == 6);
        CHECK(context, !mismatch.error().locations.front().segment.empty());
    }
    sample.fractionB = 0.95f;
    sample.consistent = true;
    CHECK(context, checkBoundaryConsistency({sample}, 0.1f).ok());
}

void testCullCheckpointCancellation(TestContext& context,
                                    const GeneratedSamples& generated) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[1]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    std::atomic<bool> cancelled{true};
    Result<FrameSnapshot> result =
        renderVisibility(read.value(), read.value().chunks, generated.camera,
                         read.value().boundaryTolerance, &cancelled);
    expectFailure(context, result, Code::Cancelled,
                  "cull checkpoint cancellation");
}

void testCompare(TestContext& context, const GeneratedSamples& generated) {
    Result<CompareReport> report =
        compareArchives(generated.archives, generated.camera, 0);
    CHECK(context, report.ok());
    if (!report.ok()) {
        return;
    }
    CHECK(context, report.value().differences.size() > 0);
    CHECK(context,
          report.value().maskHashes.at(1) != report.value().maskHashes.at(2));
    std::set<std::string> causes;
    for (const VisibilityDiff& diff : report.value().differences) {
        causes.insert(diff.cause);
        CHECK(context, diff.cause == "format_degradation" ||
                           diff.cause == "priority_derivation" ||
                           diff.cause == "load_order");
        CHECK(context, !diff.fields.empty());
    }
    CHECK(context, causes.count("format_degradation") == 1);
    CHECK(context, !report.value().metadataDifferences.empty());
    CHECK(context, !compareReportJson(report.value()).empty());
}

void testRecovery(TestContext& context,
                  const GeneratedSamples& generated,
                  const std::filesystem::path& tempDirectory) {
    Result<SceneArchive> read = readArchiveFromFile(generated.archives[2]);
    CHECK(context, read.ok());
    if (!read.ok()) {
        return;
    }
    const SceneArchive scene = read.value();
    const auto recoveryDirectory = tempDirectory / "recovery-basic";
    RecoveryStore firstStore(recoveryDirectory);
    Result<RecoveryState> empty = firstStore.recover(scene);
    CHECK(context, empty.ok());
    if (empty.ok()) {
        CHECK(context, empty.value().chunks.empty());
    }
    for (std::uint32_t index = 0; index < 3; ++index) {
        Result<CommitResult> commit =
            firstStore.commitChunk(scene, scene.chunks[index],
                                   chunkContentHash(scene.chunks[index]), 1);
        CHECK(context, commit.ok());
        if (commit.ok()) {
            CHECK(context, commit.value().wrote);
        }
    }
    CHECK(context, firstStore.writeCount() == 3);

    RecoveryStore secondStore(recoveryDirectory);
    Result<RecoveryState> recovered = secondStore.recover(scene);
    CHECK(context, recovered.ok());
    if (!recovered.ok()) {
        return;
    }
    CHECK(context, recovered.value().chunks.size() == 3);
    CHECK(context, recovered.value().nextSequence == 4);
    CHECK(context, recovered.value().chainHash != 0);

    Result<CommitResult> duplicate =
        secondStore.commitChunk(scene, scene.chunks[0],
                                chunkContentHash(scene.chunks[0]), 1);
    CHECK(context, duplicate.ok());
    if (duplicate.ok()) {
        CHECK(context, !duplicate.value().wrote);
    }

    StreamingOptions options;
    options.workerCount = 2;
    options.loadDelayMicros = 0;
    Pipeline pipeline(scene, options);
    pipeline.attachRecovery(&secondStore);
    std::vector<Chunk> seeded;
    for (const RecoveredChunk& chunk : recovered.value().chunks) {
        seeded.push_back(chunk.chunk);
    }
    pipeline.seedLoadedChunks(std::move(seeded));
    RunResult run = pipeline.tryRun(generated.camera);
    CHECK(context, run.code == Code::Ok);
    CHECK(context, run.scheduled.size() == 6);
    CHECK(context, run.snapshot != nullptr);
    if (run.snapshot) {
        CHECK(context, run.snapshot->loadedChunkIds.size() == 9);
    }
    CHECK(context, secondStore.writeCount() == 6);
    for (const auto& entry : std::filesystem::directory_iterator(recoveryDirectory)) {
        CHECK(context, entry.path().extension() != ".tmp");
    }

    RecoveryStore thirdStore(recoveryDirectory);
    Result<RecoveryState> all = thirdStore.recover(scene);
    CHECK(context, all.ok());
    if (all.ok()) {
        CHECK(context, all.value().chunks.size() == 9);
        CHECK(context, all.value().nextSequence == 10);
        for (std::size_t index = 1; index < all.value().chunks.size(); ++index) {
            CHECK(context, all.value().chunks[index].previousChainHash ==
                               all.value().chunks[index - 1].chainHash);
        }
    }

    const auto corruptDirectory = tempDirectory / "recovery-corrupt";
    RecoveryStore corruptStore(corruptDirectory);
    CHECK(context, corruptStore.recover(scene).ok());
    CHECK(context, corruptStore.commitChunk(scene, scene.chunks[0],
                                            chunkContentHash(scene.chunks[0]), 1)
                       .ok());
    CHECK(context, corruptStore.commitChunk(scene, scene.chunks[1],
                                            chunkContentHash(scene.chunks[1]), 1)
                       .ok());
    std::vector<std::filesystem::path> corruptFiles;
    for (const auto& entry : std::filesystem::directory_iterator(corruptDirectory)) {
        if (entry.path().extension() == ".snap") {
            corruptFiles.push_back(entry.path());
        }
    }
    std::sort(corruptFiles.begin(), corruptFiles.end());
    CHECK(context, corruptFiles.size() == 2);
    if (corruptFiles.size() == 2) {
        std::vector<std::uint8_t> bytes = readBytes(corruptFiles[1]);
        bytes[20] ^= 0x5a;
        writeBytes(corruptFiles[1], bytes);
        RecoveryStore broken(corruptDirectory);
        Result<RecoveryState> brokenResult = broken.recover(scene);
        expectFailure(context, brokenResult, Code::RecoveryInvalid,
                      "recovery chain", "snapshot.chain");
        if (!brokenResult.ok()) {
            CHECK(context,
                  brokenResult.error().locations.front().chunkId == 1);
        }
    }

    const auto truncatedDirectory = tempDirectory / "recovery-truncated";
    RecoveryStore truncatedStore(truncatedDirectory);
    CHECK(context, truncatedStore.recover(scene).ok());
    CHECK(context, truncatedStore.commitChunk(scene, scene.chunks[0],
                                              chunkContentHash(scene.chunks[0]), 1)
                       .ok());
    CHECK(context, truncatedStore.commitChunk(scene, scene.chunks[1],
                                              chunkContentHash(scene.chunks[1]), 1)
                       .ok());
    std::vector<std::filesystem::path> truncatedFiles;
    for (const auto& entry :
         std::filesystem::directory_iterator(truncatedDirectory)) {
        if (entry.path().extension() == ".snap") {
            truncatedFiles.push_back(entry.path());
        }
    }
    std::sort(truncatedFiles.begin(), truncatedFiles.end());
    if (truncatedFiles.size() == 2) {
        std::vector<std::uint8_t> bytes = readBytes(truncatedFiles[1]);
        bytes.resize(bytes.size() - 11);
        writeBytes(truncatedFiles[1], bytes);
        RecoveryStore broken(truncatedDirectory);
        Result<RecoveryState> brokenResult = broken.recover(scene);
        expectFailure(context, brokenResult, Code::RecoveryInvalid,
                      "recovery truncated");
    }
}

} // namespace

int main() {
    TestContext context;
    const auto stamp =
        std::chrono::high_resolution_clock::now().time_since_epoch().count();
    const std::filesystem::path tempDirectory =
        std::filesystem::temp_directory_path() /
        ("chunkpipe-tests-" + std::to_string(stamp));
    std::filesystem::remove_all(tempDirectory);
    std::filesystem::create_directories(tempDirectory);

    Result<GeneratedSamples> generated =
        generateDeterministicSamples(tempDirectory / "samples");
    if (!generated.ok()) {
        std::cerr << "sample generation failed: " << generated.error().message
                  << "\n";
        return 1;
    }

    testGeneratedArchives(context, generated.value());
    testParseFailures(context, generated.value(), tempDirectory);
    testUpgradeAndVersionLoss(context, generated.value());
    testReferenceValidation(context, generated.value());
    testPipelineAndPublishing(context, generated.value());
    testCancellationPreservesSnapshot(context, generated.value());
    testBusyBoundary(context, generated.value());
    testDeterminism(context, generated.value());
    testBoundaryMismatch(context);
    testCullCheckpointCancellation(context, generated.value());
    testCompare(context, generated.value());
    testRecovery(context, generated.value(), tempDirectory);

    std::cout << "checks=" << context.checks << " failures=" << context.failures
              << "\n";
    std::filesystem::remove_all(tempDirectory);
    return context.failures == 0 ? 0 : 1;
}

