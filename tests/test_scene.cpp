#include "test_helpers.hpp"

#include <iostream>

namespace {

preview::SceneLoadResult parseData(
    const preview::SampleSceneData& data) {
    std::vector<std::uint8_t> bytes;
    preview::Diagnostic diagnostic;
    TEST_CHECK(preview::encodeSceneBinary(data.roots, data.blocks, 0, &bytes,
                                          &diagnostic));
    return preview::parseSceneBinary(bytes);
}

preview::SceneBlock* firstLeaf(preview::SampleSceneData* data) {
    for (preview::SceneBlock& block : data->blocks) {
        if (!block.indices.empty()) {
            return &block;
        }
    }
    return nullptr;
}

void testRoundTrip() {
    const auto config = test_support::mediumSceneConfig();
    const preview::SampleSceneData generated =
        preview::generateSampleScene(config);
    const auto scene = test_support::validatedScene(generated);
    TEST_CHECK(scene != nullptr);
    TEST_CHECK(scene->roots.size() == 1);
    TEST_CHECK(scene->blocks.size() == generated.blocks.size());
    TEST_CHECK(scene->contentHash != 0);
    for (const preview::SceneBlock& block : scene->blocks) {
        TEST_CHECK(scene->findBlock(block.id) != nullptr);
        for (preview::BlockId child : block.children) {
            const preview::SceneBlock* childBlock = scene->findBlock(child);
            TEST_CHECK(childBlock != nullptr);
            TEST_CHECK(childBlock->parent == block.id);
            TEST_CHECK(block.bounds.contains(childBlock->bounds, 1e-5));
        }
    }
}

void testIndexRangeError() {
    preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    preview::SceneBlock* leaf = firstLeaf(&data);
    TEST_CHECK(leaf != nullptr);
    leaf->indices[0] = 999999u;
    const preview::SceneLoadResult loaded = parseData(data);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.blockId == leaf->id);
    TEST_CHECK(loaded.diagnostic.field.find(".indices") != std::string::npos);
}

void testDanglingParentError() {
    preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    preview::SceneBlock* leaf = firstLeaf(&data);
    TEST_CHECK(leaf != nullptr);
    leaf->parent = 999999u;
    const preview::SceneLoadResult loaded = parseData(data);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.blockId == leaf->id);
    TEST_CHECK(loaded.diagnostic.field.find(".parent") != std::string::npos);
}

void testBoundsContainmentError() {
    preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    preview::SceneBlock* leaf = firstLeaf(&data);
    TEST_CHECK(leaf != nullptr);
    leaf->bounds.max.x += 10.0;
    leaf->bounds.max.y += 10.0;
    leaf->bounds.max.z += 10.0;
    const preview::SceneLoadResult loaded = parseData(data);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.blockId == leaf->id);
    TEST_CHECK(loaded.diagnostic.field.find(".bounds") != std::string::npos);
}

void testDuplicateBlockId() {
    preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    preview::SceneBlock* leaf = firstLeaf(&data);
    TEST_CHECK(leaf != nullptr);
    const preview::BlockId duplicate = data.blocks.front().id;
    const preview::BlockId oldId = leaf->id;
    leaf->id = duplicate;
    for (preview::SceneBlock& block : data.blocks) {
        if (block.id == leaf->parent) {
            for (preview::BlockId& child : block.children) {
                if (child == oldId) {
                    child = duplicate;
                }
            }
        }
    }
    const preview::SceneLoadResult loaded = parseData(data);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.field.find(".id") != std::string::npos);
}

void testCycleError() {
    preview::SampleSceneData data;
    preview::SceneBlock a;
    a.id = 1;
    a.parent = 2;
    a.bounds.min = {0.0, 0.0, 0.0};
    a.bounds.max = {1.0, 1.0, 1.0};
    a.children = {2};
    preview::SceneBlock b = a;
    b.id = 2;
    b.parent = 1;
    b.children = {1};
    data.blocks = {a, b};
    const preview::SceneLoadResult loaded = parseData(data);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.field.find(".children") != std::string::npos);
}

void testHeaderHashError() {
    const preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    std::vector<std::uint8_t> bytes;
    preview::Diagnostic diagnostic;
    TEST_CHECK(preview::encodeSceneBinary(data.roots, data.blocks, 0, &bytes,
                                          &diagnostic));
    bytes[32] ^= 0x80u;
    const preview::SceneLoadResult loaded = preview::parseSceneBinary(bytes);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
    TEST_CHECK(loaded.diagnostic.field == "header.content_hash");
}

void testTruncatedScene() {
    const preview::SampleSceneData data =
        preview::generateSampleScene(test_support::mediumSceneConfig());
    std::vector<std::uint8_t> bytes;
    preview::Diagnostic diagnostic;
    TEST_CHECK(preview::encodeSceneBinary(data.roots, data.blocks, 0, &bytes,
                                          &diagnostic));
    bytes.resize(bytes.size() / 2);
    const preview::SceneLoadResult loaded = preview::parseSceneBinary(bytes);
    TEST_CHECK(loaded.diagnostic.code == preview::StatusCode::E_SCENE_INVALID);
}

}  // namespace

int main() {
    try {
        testRoundTrip();
        testIndexRangeError();
        testDanglingParentError();
        testBoundsContainmentError();
        testDuplicateBlockId();
        testCycleError();
        testHeaderHashError();
        testTruncatedScene();
        std::cout << "test_scene: PASS\n";
        return 0;
    } catch (const std::exception& exception) {
        std::cerr << "test_scene: FAIL: " << exception.what() << "\n";
        return 1;
    }
}
