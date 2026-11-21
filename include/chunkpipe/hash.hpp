#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "chunkpipe/model.hpp"

namespace chunkpipe {

std::uint64_t hashBytes(const void* data, std::size_t size);
std::uint64_t hashBytes(const std::vector<std::uint8_t>& bytes);
std::uint64_t hashString(std::string_view text);
std::uint64_t hashCombine(std::uint64_t seed, std::uint64_t value);
std::string hashHex(std::uint64_t value);
std::uint64_t chunkContentHash(const Chunk& chunk);
std::uint64_t sceneContentHash(const SceneArchive& scene);

} // namespace chunkpipe

