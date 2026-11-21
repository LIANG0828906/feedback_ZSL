#pragma once

#include "font_atlas/types.hpp"

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace font_atlas {

Result<std::vector<std::uint8_t>> readBinaryFile(const std::filesystem::path& path);
Result<std::string> readTextFile(const std::filesystem::path& path);
Result<void> writeFileAtomic(const std::filesystem::path& path,
                             const std::vector<std::uint8_t>& bytes);
Result<void> writeFileAtomic(const std::filesystem::path& path, const std::string& text);
Result<void> ensureDirectory(const std::filesystem::path& path);
std::string pathString(const std::filesystem::path& path);
std::string hexEncode(const std::vector<std::uint8_t>& bytes);
std::vector<std::uint8_t> bytesOf(const std::string& text);

}  // namespace font_atlas

