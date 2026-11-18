#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace caisson {

bool writePngRgbaAtomic(
    const std::filesystem::path& path,
    int width,
    int height,
    const std::vector<std::uint8_t>& rgba,
    std::string& error);

bool readPngRgba(
    const std::filesystem::path& path,
    int& width,
    int& height,
    std::vector<std::uint8_t>& rgba,
    std::string& error);

}  // namespace caisson

