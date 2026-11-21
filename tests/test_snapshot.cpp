#include "test_helpers.hpp"

#include "preview/snapshot.hpp"

#include <algorithm>
#include <csignal>
#include <fstream>
#include <iostream>
#include <iterator>

#if !defined(_WIN32)
#include <sys/wait.h>
#endif

namespace {

preview::RenderConfig snapshotRenderConfig(
    const std::filesystem::path& snapshotPath) {
    preview::RenderConfig config;
    config.pyramid.width = 128;
    config.pyramid.height = 72;
    config.pyramid.quantizationBits = 18;
    config.snapshotPath = snapshotPath.string();
    return config;
}

std::string readFile(const std::filesystem::path& path) {
    std::ifstream stream(path, std::ios::binary);
    return std::string(std::istreambuf_iterator<char>(stream),
                       std::istreambuf_iterator<char>());
}

void writeFile(const std::filesystem::path& path, const std::string& data) {
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    stream.write(data.data(), static_cast<std::streamsize>(data.size()));
}

void testSnapshotValidationAndRerenderDetection() {
    const auto sceneConfig = test_support::mediumSceneConfig();
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(sceneConfig));
    preview::StreamingLoader loader(scene, preview::StreamingConfig{4});
    const auto snapshot = test_support::loadEverything(&loader);
    const auto directory = test_support::temporaryDirectory("snapshot");
    const auto snapshotPath = directory / "state.snapshot";
    preview::Renderer renderer(loader, snapshotRenderConfig(snapshotPath));
    const preview::SampleView view = test_support::firstView(sceneConfig);
    preview::RenderRequest request;
    request.camera = view.camera;
    const preview::RenderResult rendered = renderer.render(request);
    if (!rendered) {
        throw std::runtime_error("snapshot render failed: " +
                                 rendered.diagnostic.toJson());
    }
    TEST_CHECK(std::filesystem::exists(snapshotPath));
    TEST_CHECK(!std::filesystem::exists(snapshotPath.string() + ".tmp"));

    const preview::SnapshotRecoveryCheck unchanged =
        preview::checkSnapshotRecovery(snapshotPath.string(), *snapshot,
                                       view.camera);
    if (!unchanged) {
        throw std::runtime_error("snapshot recovery failed: " +
                                 unchanged.diagnostic.toJson());
    }
    TEST_CHECK(!unchanged.requiresRerender);
    TEST_CHECK(unchanged.recoveredMaskHash == rendered.mask.contentHash);

    preview::Camera changedCamera = view.camera;
    changedCamera.position.x += 0.25;
    const preview::SnapshotRecoveryCheck viewChanged =
        preview::checkSnapshotRecovery(snapshotPath.string(), *snapshot,
                                       changedCamera);
    TEST_CHECK(viewChanged.ok());
    TEST_CHECK(viewChanged.requiresRerender);
    TEST_CHECK(std::find(viewChanged.changes.begin(), viewChanged.changes.end(),
                         "view") != viewChanged.changes.end());

    std::vector<preview::BlockId> subsetIds;
    for (std::size_t i = 0; i < snapshot->blocks.size() / 2; ++i) {
        subsetIds.push_back(snapshot->blocks[i].id);
    }
    const auto subset =
        preview::makeLoadSnapshot(scene, subsetIds, snapshot->version + 1);
    TEST_CHECK(subset.ok());
    const preview::SnapshotRecoveryCheck loadedChanged =
        preview::checkSnapshotRecovery(snapshotPath.string(), *subset.snapshot,
                                       view.camera);
    TEST_CHECK(loadedChanged.ok());
    TEST_CHECK(loadedChanged.requiresRerender);
    TEST_CHECK(std::find(loadedChanged.changes.begin(),
                         loadedChanged.changes.end(),
                         "loaded_set") != loadedChanged.changes.end());

    const std::string bytes = readFile(snapshotPath);
    TEST_CHECK(bytes.size() > 40);
    std::string corrupted = bytes;
    corrupted[40] ^= 0x55;
    const auto corruptPath = directory / "corrupt.snapshot";
    writeFile(corruptPath, corrupted);
    const preview::SnapshotLoadResult corruptResult =
        preview::loadSnapshot(corruptPath.string(),
                              preview::currentStateFromSnapshot(*snapshot,
                                                                view.camera));
    TEST_CHECK(corruptResult.code == preview::StatusCode::E_SNAPSHOT_CORRUPT);
    TEST_CHECK(corruptResult.diagnostic.field.find("checksum") !=
               std::string::npos);

    std::vector<std::uint8_t> validBytes(bytes.begin(), bytes.end());
    const preview::SnapshotLoadResult decoded =
        preview::decodeSnapshotDocument(validBytes);
    TEST_CHECK(decoded.ok());
    preview::SnapshotDocument alteredDocument = decoded.document;
    alteredDocument.loadContentHash ^= 1u;
    const std::vector<std::uint8_t> alteredBytes =
        preview::encodeSnapshotDocument(alteredDocument);
    const auto alteredPath = directory / "altered-content-hash.snapshot";
    std::ofstream alteredStream(alteredPath,
                                std::ios::binary | std::ios::trunc);
    alteredStream.write(reinterpret_cast<const char*>(alteredBytes.data()),
                        static_cast<std::streamsize>(alteredBytes.size()));
    alteredStream.close();
    const preview::SnapshotLoadResult altered =
        preview::loadSnapshot(alteredPath.string(),
                              preview::currentStateFromSnapshot(*snapshot,
                                                                view.camera));
    TEST_CHECK(altered.code == preview::StatusCode::E_SNAPSHOT_CORRUPT);
    TEST_CHECK(altered.diagnostic.field == "payload.load_content_hash");

    const preview::SnapshotLoadResult missing =
        preview::loadSnapshot((directory / "missing.snapshot").string(),
                              preview::currentStateFromSnapshot(*snapshot,
                                                                view.camera));
    TEST_CHECK(missing.code == preview::StatusCode::E_IO);

    std::filesystem::remove_all(directory);
}

#if !defined(_WIN32)
void testForcedTerminationRecovery() {
    const auto sceneConfig = test_support::mediumSceneConfig();
    const auto scene = test_support::validatedScene(
        preview::generateSampleScene(sceneConfig));
    const auto directory = test_support::temporaryDirectory("crash");
    const auto snapshotPath = directory / "crash.snapshot";
    const preview::SampleView view = test_support::firstView(sceneConfig);

    const pid_t child = fork();
    TEST_CHECK(child >= 0);
    if (child == 0) {
        try {
            preview::StreamingLoader loader(scene, preview::StreamingConfig{8});
            test_support::loadEverything(&loader);
            preview::Renderer renderer(loader,
                                       snapshotRenderConfig(snapshotPath));
            preview::RenderRequest request;
            request.camera = view.camera;
            const preview::RenderResult result = renderer.render(request);
            if (result.code != preview::StatusCode::E_OK) {
                std::_Exit(3);
            }
            std::raise(SIGKILL);
            std::_Exit(4);
        } catch (...) {
            std::_Exit(5);
        }
    }

    int status = 0;
    TEST_CHECK(waitpid(child, &status, 0) == child);
    TEST_CHECK(WIFSIGNALED(status));
    TEST_CHECK(WTERMSIG(status) == SIGKILL);
    TEST_CHECK(std::filesystem::exists(snapshotPath));

    preview::StreamingLoader recoveryLoader(scene, preview::StreamingConfig{8});
    const auto recoveredLoad = test_support::loadEverything(&recoveryLoader);
    const preview::SnapshotRecoveryCheck recovery =
        preview::checkSnapshotRecovery(snapshotPath.string(), *recoveredLoad,
                                       view.camera);
    if (!recovery) {
        throw std::runtime_error("crash snapshot could not be recovered: " +
                                 recovery.diagnostic.toJson());
    }
    TEST_CHECK(!recovery.requiresRerender);
    TEST_CHECK(recovery.recoveredSnapshotVersion > 0);
    TEST_CHECK(recovery.recoveredMaskHash != 0);

    std::filesystem::remove_all(directory);
}
#endif

}  // namespace

int main() {
    try {
        testSnapshotValidationAndRerenderDetection();
#if !defined(_WIN32)
        testForcedTerminationRecovery();
#endif
        std::cout << "test_snapshot: PASS\n";
        return 0;
    } catch (const std::exception& exception) {
        std::cerr << "test_snapshot: FAIL: " << exception.what() << "\n";
        return 1;
    }
}
