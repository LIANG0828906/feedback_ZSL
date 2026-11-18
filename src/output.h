#pragma once

#include "math.h"
#include "scene.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace cr {

struct ImageRGBA {
    int width = 0;
    int height = 0;
    std::vector<std::uint8_t> pixels;

    std::size_t byteSize() const {
        return static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 4u;
    }
};

struct OwnerInfo {
    int code = -1;
    int layer = -1;
    ComponentType type = ComponentType::Board;
    std::string componentId;
    Vec3 color{0.0, 0.0, 0.0};
};

std::map<int, OwnerInfo> buildOwnerTable(const Scene& scene);
ImageRGBA ownerImage(const std::vector<int>& owners, int width, int height,
                     const std::map<int, OwnerInfo>& table);
std::vector<std::uint8_t> ownerBinary(const std::vector<int>& owners, int width, int height);

bool writePngAtomic(const std::string& path, const ImageRGBA& image, std::string& error);
bool readPng(const std::string& path, ImageRGBA& image, std::string& error);

bool writeFileAtomic(const std::string& path, const std::vector<std::uint8_t>& bytes,
                     std::string& error);
bool writeTextAtomic(const std::string& path, const std::string& text, std::string& error);
bool readFile(const std::string& path, std::vector<std::uint8_t>& bytes, std::string& error);
bool readTextFile(const std::string& path, std::string& text, std::string& error);
bool removeFileIfExists(const std::string& path);
std::string pathJoin(const std::string& a, const std::string& b);

}  // namespace cr

