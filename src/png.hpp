#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace mosaic {

bool writeRgbaPng(
    const std::filesystem::path& path,
    std::uint32_t width,
    std::uint32_t height,
    const std::vector<std::uint8_t>& rgba,
    std::string& error);

bool writeGray16Png(
    const std::filesystem::path& path,
    std::uint32_t width,
    std::uint32_t height,
    const std::vector<std::uint16_t>& gray,
    std::string& error);

}  // namespace mosaic
