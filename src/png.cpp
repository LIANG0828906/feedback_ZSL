#include "png.hpp"

#include <algorithm>
#include <array>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <stdexcept>

namespace sl {
namespace {

std::uint32_t crc32(const std::uint8_t* data, std::size_t size) {
    std::uint32_t crc = 0xFFFFFFFFu;
    for (std::size_t i = 0; i < size; ++i) {
        crc ^= data[i];
        for (int bit = 0; bit < 8; ++bit) {
            const std::uint32_t mask = 0u - (crc & 1u);
            crc = (crc >> 1u) ^ (0xEDB88320u & mask);
        }
    }
    return crc ^ 0xFFFFFFFFu;
}

std::uint32_t adler32(const std::vector<std::uint8_t>& bytes) {
    std::uint32_t a = 1;
    std::uint32_t b = 0;
    for (std::uint8_t value : bytes) {
        a = (a + value) % 65521u;
        b = (b + a) % 65521u;
    }
    return (b << 16u) | a;
}

void putU32(std::vector<std::uint8_t>& out, std::uint32_t value) {
    out.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xFFu));
    out.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xFFu));
    out.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xFFu));
    out.push_back(static_cast<std::uint8_t>(value & 0xFFu));
}

void putChunk(std::vector<std::uint8_t>& out,
              const char type[4],
              const std::vector<std::uint8_t>& data) {
    putU32(out, static_cast<std::uint32_t>(data.size()));
    const std::size_t start = out.size();
    for (int i = 0; i < 4; ++i) {
        out.push_back(static_cast<std::uint8_t>(type[i]));
    }
    out.insert(out.end(), data.begin(), data.end());
    const std::uint32_t crc = crc32(out.data() + start, out.size() - start);
    putU32(out, crc);
}

std::vector<std::uint8_t> zlibStored(const std::vector<std::uint8_t>& input) {
    std::vector<std::uint8_t> output;
    output.reserve(input.size() + input.size() / 65535 * 5 + 16);
    output.push_back(0x78);
    output.push_back(0x01);
    std::size_t offset = 0;
    if (input.empty()) {
        output.push_back(0x01);
        output.push_back(0x00);
        output.push_back(0x00);
        output.push_back(0xFF);
        output.push_back(0xFF);
    }
    while (offset < input.size()) {
        const std::size_t length = std::min<std::size_t>(65535, input.size() - offset);
        const bool final = offset + length == input.size();
        output.push_back(final ? 0x01 : 0x00);
        const std::uint16_t len = static_cast<std::uint16_t>(length);
        const std::uint16_t nlen = static_cast<std::uint16_t>(~len);
        output.push_back(static_cast<std::uint8_t>(len & 0xFFu));
        output.push_back(static_cast<std::uint8_t>((len >> 8u) & 0xFFu));
        output.push_back(static_cast<std::uint8_t>(nlen & 0xFFu));
        output.push_back(static_cast<std::uint8_t>((nlen >> 8u) & 0xFFu));
        output.insert(output.end(), input.begin() + static_cast<std::ptrdiff_t>(offset),
                      input.begin() + static_cast<std::ptrdiff_t>(offset + length));
        offset += length;
    }
    putU32(output, adler32(input));
    return output;
}

} // namespace

void writePngRGBA(const std::string& path,
                  int width,
                  int height,
                  const std::vector<std::uint8_t>& rgba) {
    if (width <= 0 || height <= 0 ||
        rgba.size() != static_cast<std::size_t>(width * height * 4)) {
        throw std::runtime_error("invalid RGBA buffer for PNG");
    }
    std::vector<std::uint8_t> scanlines;
    scanlines.reserve(static_cast<std::size_t>(height) *
                      (1u + static_cast<std::size_t>(width) * 4u));
    for (int y = 0; y < height; ++y) {
        scanlines.push_back(0);
        const std::size_t begin = static_cast<std::size_t>(y) *
                                  static_cast<std::size_t>(width) * 4u;
        scanlines.insert(scanlines.end(),
                         rgba.begin() + static_cast<std::ptrdiff_t>(begin),
                         rgba.begin() + static_cast<std::ptrdiff_t>(
                             begin + static_cast<std::size_t>(width) * 4u));
    }

    std::vector<std::uint8_t> png = {
        0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
    };
    std::vector<std::uint8_t> ihdr;
    putU32(ihdr, static_cast<std::uint32_t>(width));
    putU32(ihdr, static_cast<std::uint32_t>(height));
    ihdr.push_back(8);
    ihdr.push_back(6);
    ihdr.push_back(0);
    ihdr.push_back(0);
    ihdr.push_back(0);
    putChunk(png, "IHDR", ihdr);
    putChunk(png, "IDAT", zlibStored(scanlines));
    putChunk(png, "IEND", {});

    std::filesystem::path target(path);
    if (!target.parent_path().empty()) {
        std::filesystem::create_directories(target.parent_path());
    }
    std::ofstream output(target, std::ios::binary | std::ios::trunc);
    if (!output) {
        throw std::runtime_error("cannot open PNG output: " + path);
    }
    output.write(reinterpret_cast<const char*>(png.data()),
                 static_cast<std::streamsize>(png.size()));
}

} // namespace sl

