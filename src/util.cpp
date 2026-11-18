#include "util.hpp"

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <limits>
#include <sstream>
#include <system_error>

#ifdef _WIN32
#include <direct.h>
#include <io.h>
#include <process.h>
#include <windows.h>
#else
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#endif

namespace sr {

std::string hexDigest(const std::array<uint8_t, 32>& bytes) {
    static const char* digits = "0123456789abcdef";
    std::string out;
    out.resize(64);
    for (size_t i = 0; i < bytes.size(); ++i) {
        out[i * 2] = digits[bytes[i] >> 4];
        out[i * 2 + 1] = digits[bytes[i] & 0x0f];
    }
    return out;
}

std::string formatDouble(double v, int precision) {
    if (!std::isfinite(v)) {
        return "0";
    }
    if (v == 0.0) {
        v = 0.0;
    }
    std::ostringstream stream;
    stream << std::fixed << std::setprecision(precision) << v;
    std::string text = stream.str();
    const auto dotPos = text.find('.');
    if (dotPos != std::string::npos) {
        while (!text.empty() && text.back() == '0') {
            text.pop_back();
        }
        if (!text.empty() && text.back() == '.') {
            text.pop_back();
        }
    }
    if (text == "-0" || text.empty()) {
        text = "0";
    }
    return text;
}

std::string formatInt(std::int64_t v) { return std::to_string(v); }

bool readFileBytes(const std::filesystem::path& path, std::vector<uint8_t>& out, std::string& error) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        error = "cannot open file: " + path.string();
        return false;
    }
    stream.seekg(0, std::ios::end);
    const std::streamoff size = stream.tellg();
    if (size < 0) {
        error = "cannot determine file size: " + path.string();
        return false;
    }
    stream.seekg(0, std::ios::beg);
    out.resize(static_cast<size_t>(size));
    if (size > 0) {
        stream.read(reinterpret_cast<char*>(out.data()), size);
    }
    if (!stream && !stream.eof()) {
        error = "cannot read file: " + path.string();
        return false;
    }
    return true;
}

bool ensureDirectory(const std::filesystem::path& path, std::string& error) {
    std::error_code ec;
    if (path.empty()) {
        return true;
    }
    std::filesystem::create_directories(path, ec);
    if (ec) {
        error = "cannot create directory " + path.string() + ": " + ec.message();
        return false;
    }
    if (!std::filesystem::is_directory(path, ec) || ec) {
        error = "path is not a directory: " + path.string();
        return false;
    }
    return true;
}

static bool syncFile(FILE* file) {
    if (std::fflush(file) != 0) {
        return false;
    }
#ifdef _WIN32
    return _commit(_fileno(file)) == 0;
#else
    return fsync(fileno(file)) == 0;
#endif
}

bool writeFileBytesAtomic(const std::filesystem::path& path, const std::vector<uint8_t>& bytes,
                          std::string& error) {
    std::error_code ec;
    const auto parent = path.parent_path();
    if (!parent.empty()) {
        std::filesystem::create_directories(parent, ec);
        if (ec) {
            error = "cannot create parent directory: " + ec.message();
            return false;
        }
    }

#ifdef _WIN32
    const int pid = _getpid();
#else
    const int pid = static_cast<int>(getpid());
#endif
    const std::filesystem::path temp =
        path.string() + ".tmp." + std::to_string(pid) + "." + std::to_string(
            static_cast<unsigned long long>(std::filesystem::file_time_type::clock::now()
                                                .time_since_epoch()
                                                .count() &
                                            0xffffffffULL));

    FILE* file = std::fopen(temp.string().c_str(), "wb");
    if (file == nullptr) {
        error = "cannot open temporary file " + temp.string() + ": " + std::strerror(errno);
        return false;
    }
    bool ok = true;
    if (!bytes.empty() &&
        std::fwrite(bytes.data(), 1, bytes.size(), file) != bytes.size()) {
        ok = false;
    }
    if (ok) {
        ok = syncFile(file);
    }
    if (std::fclose(file) != 0) {
        ok = false;
    }
    if (!ok) {
        std::remove(temp.string().c_str());
        error = "failed while writing temporary file " + temp.string();
        return false;
    }

#ifdef _WIN32
    if (!MoveFileExW(temp.wstring().c_str(), path.wstring().c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        std::remove(temp.string().c_str());
        error = "cannot atomically replace " + path.string();
        return false;
    }
#else
    if (std::rename(temp.string().c_str(), path.string().c_str()) != 0) {
        const std::string reason = std::strerror(errno);
        std::remove(temp.string().c_str());
        error = "cannot atomically replace " + path.string() + ": " + reason;
        return false;
    }
    if (!parent.empty()) {
        const int fd = open(parent.string().c_str(), O_RDONLY | O_DIRECTORY);
        if (fd >= 0) {
            (void)fsync(fd);
            close(fd);
        }
    }
#endif
    return true;
}

void appendU8(std::vector<uint8_t>& out, uint8_t v) { out.push_back(v); }

void appendU32(std::vector<uint8_t>& out, uint32_t v) {
    out.push_back(static_cast<uint8_t>(v & 0xffU));
    out.push_back(static_cast<uint8_t>((v >> 8U) & 0xffU));
    out.push_back(static_cast<uint8_t>((v >> 16U) & 0xffU));
    out.push_back(static_cast<uint8_t>((v >> 24U) & 0xffU));
}

void appendU64(std::vector<uint8_t>& out, uint64_t v) {
    for (int i = 0; i < 8; ++i) {
        out.push_back(static_cast<uint8_t>((v >> (i * 8)) & 0xffU));
    }
}

void appendDouble(std::vector<uint8_t>& out, double v) {
    static_assert(sizeof(double) == sizeof(uint64_t), "double must be 64-bit");
    uint64_t bits = 0;
    std::memcpy(&bits, &v, sizeof(bits));
    appendU64(out, bits);
}

bool readU8(const std::vector<uint8_t>& in, size_t& offset, uint8_t& v) {
    if (offset + 1 > in.size()) {
        return false;
    }
    v = in[offset++];
    return true;
}

bool readU32(const std::vector<uint8_t>& in, size_t& offset, uint32_t& v) {
    if (offset + 4 > in.size()) {
        return false;
    }
    v = static_cast<uint32_t>(in[offset]) |
        (static_cast<uint32_t>(in[offset + 1]) << 8U) |
        (static_cast<uint32_t>(in[offset + 2]) << 16U) |
        (static_cast<uint32_t>(in[offset + 3]) << 24U);
    offset += 4;
    return true;
}

bool readU64(const std::vector<uint8_t>& in, size_t& offset, uint64_t& v) {
    if (offset + 8 > in.size()) {
        return false;
    }
    v = 0;
    for (int i = 0; i < 8; ++i) {
        v |= static_cast<uint64_t>(in[offset + static_cast<size_t>(i)]) << (i * 8);
    }
    offset += 8;
    return true;
}

bool readDouble(const std::vector<uint8_t>& in, size_t& offset, double& v) {
    uint64_t bits = 0;
    if (!readU64(in, offset, bits)) {
        return false;
    }
    std::memcpy(&v, &bits, sizeof(v));
    return true;
}

}  // namespace sr

