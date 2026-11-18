#pragma once

#include "util.hpp"

#include <cstdint>
#include <filesystem>
#include <vector>

namespace sr {

struct RgbaImage {
    int width = 0;
    int height = 0;
    std::vector<uint8_t> pixels;
};

bool writePngAtomic(const std::filesystem::path& path, const RgbaImage& image,
                    std::string& error);

RgbaImage ownershipImage(const std::vector<uint32_t>& ownership, int width, int height);

}  // namespace sr

