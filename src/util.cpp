#include "util.h"

#include <cmath>
#include <cerrno>
#include <cstring>
#include <cstdio>
#include <fstream>
#include <iomanip>
#include <sstream>
#include <system_error>

#ifdef _WIN32
#include <windows.h>
#endif

namespace caisson {
namespace {

std::filesystem::path temporaryPath(const std::filesystem::path& path) {
    std::filesystem::path result = path;
    result += ".tmp";
    return result;
}

}  // namespace

std::string StructuredError::toJson() const {
    std::ostringstream out;
    out << "{\"error\":{\"code\":" << quote(code)
        << ",\"message\":" << quote(message);
    if (layerId >= 0) out << ",\"layer_id\":" << layerId;
    if (quadrant >= 0) out << ",\"quadrant\":" << quadrant;
    if (!objectId.empty()) out << ",\"object_id\":" << quote(objectId);
    out << "}}";
    return out.str();
}

bool readTextFile(const std::filesystem::path& path, std::string& output, std::string& error) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        error = "cannot open file for reading";
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        error = "failed while reading file";
        return false;
    }
    output = buffer.str();
    return true;
}

bool readBinaryFile(
    const std::filesystem::path& path,
    std::vector<std::uint8_t>& output,
    std::string& error) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input) {
        error = "cannot open file for reading";
        return false;
    }
    const std::streamoff size = input.tellg();
    if (size < 0) {
        error = "cannot determine file size";
        return false;
    }
    output.resize(static_cast<std::size_t>(size));
    input.seekg(0, std::ios::beg);
    if (!output.empty()) {
        input.read(reinterpret_cast<char*>(output.data()), size);
    }
    if (!input) {
        error = "failed while reading file";
        return false;
    }
    return true;
}

bool writeBinaryFile(
    const std::filesystem::path& path,
    const std::vector<std::uint8_t>& bytes,
    std::string& error) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) {
        error = "cannot open file for writing";
        return false;
    }
    if (!bytes.empty()) {
        output.write(reinterpret_cast<const char*>(bytes.data()),
                     static_cast<std::streamsize>(bytes.size()));
    }
    output.flush();
    if (!output) {
        error = "failed while writing file";
        return false;
    }
    return true;
}

bool replaceFile(
    const std::filesystem::path& temporary,
    const std::filesystem::path& destination,
    std::string& error) {
#ifdef _WIN32
    if (!MoveFileExW(
            temporary.wstring().c_str(),
            destination.wstring().c_str(),
            MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        error = "MoveFileExW failed";
        return false;
    }
    return true;
#else
    if (std::rename(temporary.c_str(), destination.c_str()) != 0) {
        error = std::string("rename failed: ") + std::strerror(errno);
        return false;
    }
    return true;
#endif
}

bool atomicWriteBinary(
    const std::filesystem::path& path,
    const std::vector<std::uint8_t>& bytes,
    std::string& error) {
    std::error_code ec;
    std::filesystem::create_directories(path.parent_path(), ec);
    if (ec) {
        error = "cannot create parent directory";
        return false;
    }
    const std::filesystem::path temp = temporaryPath(path);
    if (!writeBinaryFile(temp, bytes, error)) return false;
    if (!replaceFile(temp, path, error)) {
        std::filesystem::remove(temp, ec);
        return false;
    }
    return true;
}

bool atomicWriteText(
    const std::filesystem::path& path,
    const std::string& text,
    std::string& error) {
    std::vector<std::uint8_t> bytes(text.begin(), text.end());
    return atomicWriteBinary(path, bytes, error);
}

bool ensureDirectory(const std::filesystem::path& path, std::string& error) {
    std::error_code ec;
    std::filesystem::create_directories(path, ec);
    if (ec) {
        error = "cannot create directory: " + ec.message();
        return false;
    }
    return true;
}

std::string formatDouble(double value) {
    if (!std::isfinite(value)) return "0";
    if (std::abs(value) < 5.0e-18) value = 0.0;
    std::ostringstream out;
    out << std::setprecision(17) << std::defaultfloat << value;
    return out.str();
}

std::string formatFixed(double value, int precision) {
    if (!std::isfinite(value)) return "0";
    std::ostringstream out;
    out << std::fixed << std::setprecision(precision) << value;
    return out.str();
}

std::string pathForReport(const std::filesystem::path& relative) {
    return relative.generic_string();
}

std::string quote(const std::string& value) {
    return "\"" + json::escape(value) + "\"";
}

std::uint64_t fnv1a64(const void* data, std::size_t size) {
    const auto* bytes = static_cast<const std::uint8_t*>(data);
    std::uint64_t hash = 1469598103934665603ULL;
    for (std::size_t i = 0; i < size; ++i) {
        hash ^= bytes[i];
        hash *= 1099511628211ULL;
    }
    return hash;
}

std::uint64_t fnv1a64(const std::string& text) {
    return fnv1a64(text.data(), text.size());
}

}  // namespace caisson

