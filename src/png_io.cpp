#include "png_io.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <limits>

#include <png.h>

#include "util.h"

namespace caisson {
namespace {

std::filesystem::path pngTemporaryPath(const std::filesystem::path& path) {
    std::filesystem::path temp = path;
    temp += ".tmp";
    return temp;
}

FILE* openPngFile(const std::filesystem::path& path, const wchar_t* mode) {
#ifdef _WIN32
    return _wfopen(path.wstring().c_str(), mode);
#else
    (void)mode;
    return std::fopen(path.string().c_str(), "rb");
#endif
}

bool writePng(
    const std::filesystem::path& path,
    int width,
    int height,
    const std::vector<std::uint8_t>& rgba,
    std::string& error) {
    FILE* file =
#ifdef _WIN32
        openPngFile(path, L"wb");
#else
        std::fopen(path.string().c_str(), "wb");
#endif
    if (file == nullptr) {
        error = "cannot open PNG for writing";
        return false;
    }

    png_structp png = png_create_write_struct(PNG_LIBPNG_VER_STRING, nullptr, nullptr, nullptr);
    if (png == nullptr) {
        std::fclose(file);
        error = "png_create_write_struct failed";
        return false;
    }
    png_infop info = png_create_info_struct(png);
    if (info == nullptr) {
        png_destroy_write_struct(&png, nullptr);
        std::fclose(file);
        error = "png_create_info_struct failed";
        return false;
    }
    if (setjmp(png_jmpbuf(png)) != 0) {
        png_destroy_write_struct(&png, &info);
        std::fclose(file);
        std::filesystem::remove(path);
        error = "libpng write error";
        return false;
    }

    png_init_io(png, file);
    png_set_IHDR(
        png,
        info,
        static_cast<png_uint_32>(width),
        static_cast<png_uint_32>(height),
        8,
        PNG_COLOR_TYPE_RGBA,
        PNG_INTERLACE_NONE,
        PNG_COMPRESSION_TYPE_DEFAULT,
        PNG_FILTER_TYPE_DEFAULT);
    png_set_compression_level(png, 6);
    png_set_filter(png, PNG_FILTER_TYPE_BASE, PNG_FILTER_NONE);
    png_write_info(png, info);

    std::vector<png_bytep> rows(static_cast<std::size_t>(height));
    for (int y = 0; y < height; ++y) {
        rows[static_cast<std::size_t>(y)] =
            const_cast<png_bytep>(rgba.data() + static_cast<std::size_t>(y) *
                                                    static_cast<std::size_t>(width) * 4);
    }
    png_write_image(png, rows.data());
    png_write_end(png, info);
    png_destroy_write_struct(&png, &info);
    if (std::fclose(file) != 0) {
        error = "failed to close PNG file";
        return false;
    }
    return true;
}

}  // namespace

bool writePngRgbaAtomic(
    const std::filesystem::path& path,
    int width,
    int height,
    const std::vector<std::uint8_t>& rgba,
    std::string& error) {
    if (width <= 0 || height <= 0) {
        error = "invalid PNG dimensions";
        return false;
    }
    const std::size_t expected =
        static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 4;
    if (rgba.size() != expected) {
        error = "RGBA buffer size does not match dimensions";
        return false;
    }
    std::error_code ec;
    std::filesystem::create_directories(path.parent_path(), ec);
    if (ec) {
        error = "cannot create PNG parent directory";
        return false;
    }
    const std::filesystem::path temp = pngTemporaryPath(path);
    if (!writePng(temp, width, height, rgba, error)) return false;
    if (!replaceFile(temp, path, error)) {
        std::filesystem::remove(temp, ec);
        return false;
    }
    return true;
}

bool readPngRgba(
    const std::filesystem::path& path,
    int& width,
    int& height,
    std::vector<std::uint8_t>& rgba,
    std::string& error) {
    FILE* file = openPngFile(path, L"rb");
    if (file == nullptr) {
        error = "cannot open PNG for reading";
        return false;
    }
    png_byte header[8]{};
    if (std::fread(header, 1, 8, file) != 8 || png_sig_cmp(header, 0, 8) != 0) {
        std::fclose(file);
        error = "file is not a PNG";
        return false;
    }

    png_structp png = png_create_read_struct(PNG_LIBPNG_VER_STRING, nullptr, nullptr, nullptr);
    if (png == nullptr) {
        std::fclose(file);
        error = "png_create_read_struct failed";
        return false;
    }
    png_infop info = png_create_info_struct(png);
    if (info == nullptr) {
        png_destroy_read_struct(&png, nullptr, nullptr);
        std::fclose(file);
        error = "png_create_info_struct failed";
        return false;
    }
    if (setjmp(png_jmpbuf(png)) != 0) {
        png_destroy_read_struct(&png, &info, nullptr);
        std::fclose(file);
        error = "libpng read error";
        return false;
    }

    png_init_io(png, file);
    png_set_sig_bytes(png, 8);
    png_read_info(png, info);
    const png_uint_32 pngWidth = png_get_image_width(png, info);
    png_uint_32 pngHeight = png_get_image_height(png, info);
    const int bitDepth = png_get_bit_depth(png, info);
    const int colorType = png_get_color_type(png, info);
    if (pngWidth == 0 || pngHeight == 0 ||
        pngWidth > static_cast<png_uint_32>(std::numeric_limits<int>::max()) ||
        pngHeight > static_cast<png_uint_32>(std::numeric_limits<int>::max())) {
        png_destroy_read_struct(&png, &info, nullptr);
        std::fclose(file);
        error = "PNG dimensions are invalid";
        return false;
    }

    if (bitDepth == 16) png_set_strip_16(png);
    if (colorType == PNG_COLOR_TYPE_PALETTE) png_set_palette_to_rgb(png);
    if (bitDepth < 8) png_set_expand_gray_1_2_4_to_8(png);
    if (colorType == PNG_COLOR_TYPE_GRAY || colorType == PNG_COLOR_TYPE_GRAY_ALPHA) {
        png_set_gray_to_rgb(png);
    }
    if (png_get_valid(png, info, PNG_INFO_tRNS)) png_set_tRNS_to_alpha(png);
    png_set_add_alpha(png, 0xFF, PNG_FILLER_AFTER);
    png_set_interlace_handling(png);
    png_read_update_info(png, info);

    if (png_get_channels(png, info) != 4 ||
        png_get_rowbytes(png, info) !=
            static_cast<png_size_t>(pngWidth) * 4U) {
        png_destroy_read_struct(&png, &info, nullptr);
        std::fclose(file);
        error = "PNG could not be converted to RGBA8";
        return false;
    }

    width = static_cast<int>(pngWidth);
    height = static_cast<int>(pngHeight);
    rgba.assign(
        static_cast<std::size_t>(width) * static_cast<std::size_t>(height) * 4,
        0U);
    std::vector<png_bytep> rows(static_cast<std::size_t>(height));
    for (int y = 0; y < height; ++y) {
        rows[static_cast<std::size_t>(y)] =
            rgba.data() + static_cast<std::size_t>(y) *
                              static_cast<std::size_t>(width) * 4;
    }
    png_read_image(png, rows.data());
    png_read_end(png, info);
    png_destroy_read_struct(&png, &info, nullptr);
    std::fclose(file);
    return true;
}

}  // namespace caisson

