#include "output.h"

#include "hash.h"

#include <algorithm>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <sstream>

#define STB_IMAGE_WRITE_IMPLEMENTATION
#include "stb_image_write.h"
#define STB_IMAGE_IMPLEMENTATION
#include "stb_image.h"

#ifdef _WIN32
#include <windows.h>
#endif

namespace cr {
namespace {

std::vector<std::uint8_t> packLittleInt32(std::int32_t value) {
    const std::uint32_t bits = static_cast<std::uint32_t>(value);
    return {static_cast<std::uint8_t>(bits & 0xffu),
            static_cast<std::uint8_t>((bits >> 8u) & 0xffu),
            static_cast<std::uint8_t>((bits >> 16u) & 0xffu),
            static_cast<std::uint8_t>((bits >> 24u) & 0xffu)};
}

}  // namespace

std::map<int, OwnerInfo> buildOwnerTable(const Scene& scene) {
    std::map<int, OwnerInfo> table;
    table[-1] = {-1, -1, ComponentType::Board, "<background>", {0.0, 0.0, 0.0}};
    table[0] = {0, -1, ComponentType::Board, "<receiver>", {1.0, 1.0, 1.0}};
    for (const Layer& layer : scene.layers) {
        for (const Board& board : layer.boards) {
            OwnerInfo info;
            info.code = board.ownerCode;
            info.layer = layer.index;
            info.type = ComponentType::Board;
            info.componentId = board.id;
            const std::string key = std::to_string(layer.index) + "|board|" + board.id;
            const Digest digest = sha256(key);
            info.color = {static_cast<double>(digest[0]) / 255.0,
                          static_cast<double>(digest[1]) / 255.0,
                          static_cast<double>(digest[2]) / 255.0};
            table[info.code] = info;
        }
        for (const Bracket& bracket : layer.brackets) {
            OwnerInfo info;
            info.code = bracket.ownerCode;
            info.layer = layer.index;
            info.type = ComponentType::Bracket;
            info.componentId = bracket.id;
            const std::string key = std::to_string(layer.index) + "|bracket|" + bracket.id;
            const Digest digest = sha256(key);
            info.color = {static_cast<double>(digest[0]) / 255.0,
                          static_cast<double>(digest[1]) / 255.0,
                          static_cast<double>(digest[2]) / 255.0};
            if (length(info.color) < 0.08) info.color = {0.25, 0.25, 0.25};
            table[info.code] = info;
        }
    }
    return table;
}

ImageRGBA ownerImage(const std::vector<int>& owners, int width, int height,
                     const std::map<int, OwnerInfo>& table) {
    ImageRGBA image;
    image.width = width;
    image.height = height;
    image.pixels.resize(static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 4u);
    for (std::size_t i = 0;
         i < static_cast<std::size_t>(width) * static_cast<std::size_t>(height); ++i) {
        const int code = owners[i];
        const auto it = table.find(code);
        const Vec3 color = it == table.end() ? Vec3{1.0, 0.0, 1.0} : it->second.color;
        image.pixels[i * 4] = static_cast<std::uint8_t>(
            std::lround(clamp01(color.x) * 255.0));
        image.pixels[i * 4 + 1] = static_cast<std::uint8_t>(
            std::lround(clamp01(color.y) * 255.0));
        image.pixels[i * 4 + 2] = static_cast<std::uint8_t>(
            std::lround(clamp01(color.z) * 255.0));
        image.pixels[i * 4 + 3] = 255u;
    }
    return image;
}

std::vector<std::uint8_t> ownerBinary(const std::vector<int>& owners, int width, int height) {
    const std::string header = "COFFOWN1\n" + std::to_string(width) + " " +
                               std::to_string(height) + "\n";
    std::vector<std::uint8_t> bytes(header.begin(), header.end());
    bytes.reserve(header.size() + owners.size() * 4);
    for (const int owner : owners) {
        const std::vector<std::uint8_t> packed = packLittleInt32(owner);
        bytes.insert(bytes.end(), packed.begin(), packed.end());
    }
    return bytes;
}

bool writePngAtomic(const std::string& path, const ImageRGBA& image, std::string& error) {
    if (image.width <= 0 || image.height <= 0 ||
        image.pixels.size() != image.byteSize()) {
        error = "invalid RGBA image for PNG write: " + path;
        return false;
    }
    const std::string temporary = path + ".tmp";
    const int ok = stbi_write_png(temporary.c_str(), image.width, image.height, 4,
                                  image.pixels.data(), image.width * 4);
    if (ok == 0) {
        error = "stb_image_write failed for " + path;
        std::remove(temporary.c_str());
        return false;
    }
#ifdef _WIN32
    const std::filesystem::path from = std::filesystem::u8path(temporary);
    const std::filesystem::path to = std::filesystem::u8path(path);
    if (!MoveFileExW(from.wstring().c_str(), to.wstring().c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        error = "MoveFileExW failed for " + path;
        return false;
    }
#else
    std::error_code ec;
    std::filesystem::rename(temporary, path, ec);
    if (ec) {
        error = "rename failed for " + path + ": " + ec.message();
        return false;
    }
#endif
    return true;
}

bool readPng(const std::string& path, ImageRGBA& image, std::string& error) {
    int width = 0;
    int height = 0;
    int channels = 0;
    stbi_uc* data = stbi_load(path.c_str(), &width, &height, &channels, 4);
    if (data == nullptr) {
        error = "stbi_load failed for " + path;
        return false;
    }
    image.width = width;
    image.height = height;
    image.pixels.assign(data, data + static_cast<std::size_t>(width) *
                                          static_cast<std::size_t>(height) * 4u);
    stbi_image_free(data);
    return true;
}

bool writeFileAtomic(const std::string& path, const std::vector<std::uint8_t>& bytes,
                     std::string& error) {
    const std::filesystem::path target(path);
    std::error_code ec;
    if (target.has_parent_path()) {
        std::filesystem::create_directories(target.parent_path(), ec);
        if (ec) {
            error = "cannot create output directory " + target.parent_path().string() + ": " +
                    ec.message();
            return false;
        }
    }
    const std::string temporary = path + ".tmp";
    {
        std::ofstream output(temporary, std::ios::binary | std::ios::trunc);
        if (!output) {
            error = "cannot open temporary file " + temporary;
            return false;
        }
        output.write(reinterpret_cast<const char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()));
        output.flush();
        if (!output) {
            error = "cannot write temporary file " + temporary;
            return false;
        }
    }
#ifdef _WIN32
    const std::filesystem::path from = std::filesystem::u8path(temporary);
    const std::filesystem::path to = std::filesystem::u8path(path);
    if (!MoveFileExW(from.wstring().c_str(), to.wstring().c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        error = "MoveFileExW failed for " + path;
        return false;
    }
#else
    std::filesystem::rename(temporary, path, ec);
    if (ec) {
        error = "rename failed for " + path + ": " + ec.message();
        return false;
    }
#endif
    return true;
}

bool writeTextAtomic(const std::string& path, const std::string& text, std::string& error) {
    return writeFileAtomic(path, std::vector<std::uint8_t>(text.begin(), text.end()), error);
}

bool readFile(const std::string& path, std::vector<std::uint8_t>& bytes, std::string& error) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        error = "cannot open file " + path;
        return false;
    }
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    if (size < 0) {
        error = "cannot determine file size " + path;
        return false;
    }
    input.seekg(0, std::ios::beg);
    bytes.resize(static_cast<std::size_t>(size));
    input.read(reinterpret_cast<char*>(bytes.data()), size);
    if (!input) {
        error = "cannot read file " + path;
        return false;
    }
    return true;
}

bool readTextFile(const std::string& path, std::string& text, std::string& error) {
    std::vector<std::uint8_t> bytes;
    if (!readFile(path, bytes, error)) return false;
    text.assign(bytes.begin(), bytes.end());
    return true;
}

bool removeFileIfExists(const std::string& path) {
    std::error_code ec;
    if (!std::filesystem::exists(path, ec)) return true;
    return std::filesystem::remove(path, ec) && !ec;
}

std::string pathJoin(const std::string& a, const std::string& b) {
    return (std::filesystem::path(a) / std::filesystem::path(b)).string();
}

}  // namespace cr

