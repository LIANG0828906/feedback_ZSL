#include "image.hpp"

#include "sha256.hpp"

#include <cstring>
#include <vector>

namespace sr {
namespace {

void appendU32Be(std::vector<uint8_t>& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>((value >> 24U) & 0xffU));
    out.push_back(static_cast<uint8_t>((value >> 16U) & 0xffU));
    out.push_back(static_cast<uint8_t>((value >> 8U) & 0xffU));
    out.push_back(static_cast<uint8_t>(value & 0xffU));
}

void appendChunk(std::vector<uint8_t>& png, const char type[4],
                 const std::vector<uint8_t>& data) {
    appendU32Be(png, static_cast<uint32_t>(data.size()));
    const size_t typeOffset = png.size();
    png.insert(png.end(), type, type + 4);
    png.insert(png.end(), data.begin(), data.end());
    const uint32_t checksum = crc32(png.data() + typeOffset, 4U + data.size());
    appendU32Be(png, checksum);
}

std::vector<uint8_t> zlibStored(const std::vector<uint8_t>& raw) {
    std::vector<uint8_t> zlib;
    zlib.reserve(raw.size() + raw.size() / 65535U * 5U + 16U);
    zlib.push_back(0x78U);
    zlib.push_back(0x01U);
    size_t offset = 0;
    do {
        const size_t blockSize = std::min<size_t>(65535U, raw.size() - offset);
        const bool finalBlock = offset + blockSize >= raw.size();
        zlib.push_back(finalBlock ? 0x01U : 0x00U);
        const uint16_t length = static_cast<uint16_t>(blockSize);
        const uint16_t complement = static_cast<uint16_t>(~length);
        zlib.push_back(static_cast<uint8_t>(length & 0xffU));
        zlib.push_back(static_cast<uint8_t>((length >> 8U) & 0xffU));
        zlib.push_back(static_cast<uint8_t>(complement & 0xffU));
        zlib.push_back(static_cast<uint8_t>((complement >> 8U) & 0xffU));
        if (blockSize > 0) {
            zlib.insert(zlib.end(), raw.begin() + static_cast<std::ptrdiff_t>(offset),
                        raw.begin() + static_cast<std::ptrdiff_t>(offset + blockSize));
        }
        offset += blockSize;
    } while (offset < raw.size());
    const uint32_t checksum = adler32(raw.data(), raw.size());
    appendU32Be(zlib, checksum);
    return zlib;
}

}  // namespace

bool writePngAtomic(const std::filesystem::path& path, const RgbaImage& image,
                    std::string& error) {
    if (image.width <= 0 || image.height <= 0 ||
        image.pixels.size() !=
            static_cast<size_t>(image.width) * static_cast<size_t>(image.height) * 4U) {
        error = "invalid RGBA image buffer";
        return false;
    }
    std::vector<uint8_t> raw;
    raw.reserve((static_cast<size_t>(image.width) * 4U + 1U) *
                static_cast<size_t>(image.height));
    for (int y = 0; y < image.height; ++y) {
        raw.push_back(0U);
        const size_t rowOffset =
            static_cast<size_t>(y) * static_cast<size_t>(image.width) * 4U;
        raw.insert(raw.end(),
                   image.pixels.begin() + static_cast<std::ptrdiff_t>(rowOffset),
                   image.pixels.begin() +
                       static_cast<std::ptrdiff_t>(
                           rowOffset + static_cast<size_t>(image.width) * 4U));
    }

    std::vector<uint8_t> png = {0x89U, 0x50U, 0x4eU, 0x47U,
                                0x0dU, 0x0aU, 0x1aU, 0x0aU};
    std::vector<uint8_t> ihdr;
    appendU32Be(ihdr, static_cast<uint32_t>(image.width));
    appendU32Be(ihdr, static_cast<uint32_t>(image.height));
    ihdr.push_back(8U);
    ihdr.push_back(6U);
    ihdr.push_back(0U);
    ihdr.push_back(0U);
    ihdr.push_back(0U);
    appendChunk(png, "IHDR", ihdr);
    appendChunk(png, "IDAT", zlibStored(raw));
    appendChunk(png, "IEND", {});
    return writeFileBytesAtomic(path, png, error);
}

RgbaImage ownershipImage(const std::vector<uint32_t>& ownership, int width, int height) {
    RgbaImage image;
    image.width = width;
    image.height = height;
    image.pixels.resize(ownership.size() * 4U);
    for (size_t i = 0; i < ownership.size(); ++i) {
        const uint32_t id = ownership[i];
        image.pixels[i * 4U] = static_cast<uint8_t>(id & 0xffU);
        image.pixels[i * 4U + 1U] = static_cast<uint8_t>((id >> 8U) & 0xffU);
        image.pixels[i * 4U + 2U] = static_cast<uint8_t>((id >> 16U) & 0xffU);
        image.pixels[i * 4U + 3U] = static_cast<uint8_t>((id >> 24U) & 0xffU);
    }
    return image;
}

}  // namespace sr

