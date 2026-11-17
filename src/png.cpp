#include "png.hpp"

#include "snapshot.hpp"

#include <algorithm>
#include <array>
#include <cstdint>
#include <span>
#include <string>
#include <vector>

namespace mosaic {

namespace {

constexpr std::array<std::uint32_t, 256> makeCrcTable() {
    std::array<std::uint32_t, 256> table{};
    for (std::uint32_t i = 0; i < 256; ++i) {
        std::uint32_t value = i;
        for (int bit = 0; bit < 8; ++bit) {
            value = (value & 1U) != 0U ? 0xedb88320U ^ (value >> 1U) : value >> 1U;
        }
        table[i] = value;
    }
    return table;
}

constexpr auto kCrcTable = makeCrcTable();

std::uint32_t crc32Update(std::uint32_t crc, std::span<const std::uint8_t> bytes) {
    for (const std::uint8_t value : bytes) {
        crc = kCrcTable[(crc ^ value) & 0xFFU] ^ (crc >> 8U);
    }
    return crc;
}

void appendU32(std::vector<std::uint8_t>& out, std::uint32_t value) {
    out.push_back(static_cast<std::uint8_t>((value >> 24U) & 0xFFU));
    out.push_back(static_cast<std::uint8_t>((value >> 16U) & 0xFFU));
    out.push_back(static_cast<std::uint8_t>((value >> 8U) & 0xFFU));
    out.push_back(static_cast<std::uint8_t>(value & 0xFFU));
}

void appendChunk(
    std::vector<std::uint8_t>& png,
    const char type[4],
    std::span<const std::uint8_t> data) {
    appendU32(png, static_cast<std::uint32_t>(data.size()));
    const auto* typeBytes = reinterpret_cast<const std::uint8_t*>(type);
    std::uint32_t crc = crc32Update(0xffffffffU, std::span(typeBytes, 4));
    crc = crc32Update(crc, data) ^ 0xffffffffU;
    png.insert(png.end(), typeBytes, typeBytes + 4);
    png.insert(png.end(), data.begin(), data.end());
    appendU32(png, crc);
}

std::uint32_t adler32(std::span<const std::uint8_t> bytes) {
    std::uint32_t a = 1;
    std::uint32_t b = 0;
    for (const std::uint8_t value : bytes) {
        a = (a + value) % 65521U;
        b = (b + a) % 65521U;
    }
    return (b << 16U) | a;
}

std::vector<std::uint8_t> zlibStored(std::span<const std::uint8_t> bytes) {
    std::vector<std::uint8_t> output;
    output.push_back(0x78U);
    output.push_back(0x01U);
    std::size_t offset = 0;
    while (offset < bytes.size() || (bytes.empty() && offset == 0)) {
        const std::size_t blockSize = std::min<std::size_t>(65535, bytes.size() - offset);
        const bool finalBlock = offset + blockSize >= bytes.size();
        output.push_back(finalBlock ? 0x01U : 0x00U);
        output.push_back(static_cast<std::uint8_t>(blockSize & 0xFFU));
        output.push_back(static_cast<std::uint8_t>((blockSize >> 8U) & 0xFFU));
        const std::uint16_t inverse = static_cast<std::uint16_t>(~blockSize);
        output.push_back(static_cast<std::uint8_t>(inverse & 0xFFU));
        output.push_back(static_cast<std::uint8_t>((inverse >> 8U) & 0xFFU));
        output.insert(
            output.end(),
            bytes.begin() + static_cast<std::ptrdiff_t>(offset),
            bytes.begin() + static_cast<std::ptrdiff_t>(offset + blockSize));
        offset += blockSize;
        if (bytes.empty()) {
            break;
        }
    }
    appendU32(output, adler32(bytes));
    return output;
}

bool writePng(
    const std::filesystem::path& path,
    std::uint32_t width,
    std::uint32_t height,
    std::uint8_t bitDepth,
    std::uint8_t colorType,
    std::span<const std::uint8_t> scanlines,
    std::string& error) {
    std::vector<std::uint8_t> png = {
        0x89U, 0x50U, 0x4eU, 0x47U, 0x0dU, 0x0aU, 0x1aU, 0x0aU,
    };
    std::vector<std::uint8_t> header;
    appendU32(header, width);
    appendU32(header, height);
    header.push_back(bitDepth);
    header.push_back(colorType);
    header.push_back(0U);
    header.push_back(0U);
    header.push_back(0U);
    appendChunk(png, "IHDR", header);
    const std::vector<std::uint8_t> compressed = zlibStored(scanlines);
    appendChunk(png, "IDAT", compressed);
    appendChunk(png, "IEND", {});
    return atomicWriteFile(path, png, error);
}

}  // namespace

bool writeRgbaPng(
    const std::filesystem::path& path,
    std::uint32_t width,
    std::uint32_t height,
    const std::vector<std::uint8_t>& rgba,
    std::string& error) {
    if (rgba.size() != static_cast<std::size_t>(width) * height * 4U) {
        error = "RGBA buffer size does not match PNG dimensions";
        return false;
    }
    std::vector<std::uint8_t> scanlines;
    scanlines.reserve(static_cast<std::size_t>(height) * (width * 4U + 1U));
    for (std::uint32_t y = 0; y < height; ++y) {
        scanlines.push_back(0U);
        const std::size_t offset = static_cast<std::size_t>(y) * width * 4U;
        scanlines.insert(
            scanlines.end(),
            rgba.begin() + static_cast<std::ptrdiff_t>(offset),
            rgba.begin() + static_cast<std::ptrdiff_t>(offset + width * 4U));
    }
    return writePng(path, width, height, 8, 6, scanlines, error);
}

bool writeGray16Png(
    const std::filesystem::path& path,
    std::uint32_t width,
    std::uint32_t height,
    const std::vector<std::uint16_t>& gray,
    std::string& error) {
    if (gray.size() != static_cast<std::size_t>(width) * height) {
        error = "gray buffer size does not match PNG dimensions";
        return false;
    }
    std::vector<std::uint8_t> scanlines;
    scanlines.reserve(static_cast<std::size_t>(height) * (width * 2U + 1U));
    for (std::uint32_t y = 0; y < height; ++y) {
        scanlines.push_back(0U);
        for (std::uint32_t x = 0; x < width; ++x) {
            const std::uint16_t value =
                gray[static_cast<std::size_t>(y) * width + x];
            scanlines.push_back(static_cast<std::uint8_t>((value >> 8U) & 0xFFU));
            scanlines.push_back(static_cast<std::uint8_t>(value & 0xFFU));
        }
    }
    return writePng(path, width, height, 16, 0, scanlines, error);
}

}  // namespace mosaic
