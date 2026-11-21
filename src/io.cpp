#include "font_atlas/io.hpp"

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fstream>
#include <sstream>

#ifdef _WIN32
#include <windows.h>
#include <io.h>
#else
#include <fcntl.h>
#include <unistd.h>
#endif

namespace font_atlas {
namespace {

Diagnostic ioError(const std::string& message, const std::filesystem::path& path) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::IoError;
    diagnostic.message = message + ": " + pathString(path);
    return diagnostic;
}

Result<void> writeDurable(const std::filesystem::path& path,
                          const std::vector<std::uint8_t>& bytes) {
    const std::string pathText = pathString(path);
#ifdef _WIN32
    FILE* file = nullptr;
    if (fopen_s(&file, pathText.c_str(), "wb") != 0 || file == nullptr) {
        return Result<void>::failure(ioError("cannot create file", path));
    }
#else
    FILE* file = std::fopen(pathText.c_str(), "wb");
    if (file == nullptr) {
        return Result<void>::failure(ioError("cannot create file", path));
    }
#endif
    std::size_t written = 0;
    if (!bytes.empty()) {
        written = std::fwrite(bytes.data(), 1, bytes.size(), file);
    }
    if (written != bytes.size()) {
        std::fclose(file);
        return Result<void>::failure(ioError("short write to file", path));
    }
    if (std::fflush(file) != 0) {
        std::fclose(file);
        return Result<void>::failure(ioError("cannot flush file", path));
    }
#ifdef _WIN32
    const int descriptor = _fileno(file);
    if (_commit(descriptor) != 0) {
        std::fclose(file);
        return Result<void>::failure(ioError("cannot commit file", path));
    }
#else
    const int descriptor = fileno(file);
    if (::fsync(descriptor) != 0) {
        std::fclose(file);
        return Result<void>::failure(ioError("cannot fsync file", path));
    }
#endif
    if (std::fclose(file) != 0) {
        return Result<void>::failure(ioError("cannot close file", path));
    }
    return Result<void>::success();
}

}  // namespace

Result<std::vector<std::uint8_t>> readBinaryFile(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        return Result<std::vector<std::uint8_t>>::failure(
            ioError("cannot open file", path));
    }
    input.seekg(0, std::ios::end);
    const std::streamoff length = input.tellg();
    if (length < 0) {
        return Result<std::vector<std::uint8_t>>::failure(
            ioError("cannot determine file size", path));
    }
    input.seekg(0, std::ios::beg);
    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(length));
    if (!bytes.empty()) {
        input.read(reinterpret_cast<char*>(bytes.data()), length);
    }
    if (!input.good() && !input.eof()) {
        return Result<std::vector<std::uint8_t>>::failure(
            ioError("cannot read file", path));
    }
    return Result<std::vector<std::uint8_t>>::success(std::move(bytes));
}

Result<std::string> readTextFile(const std::filesystem::path& path) {
    auto bytes = readBinaryFile(path);
    if (!bytes.ok()) {
        return Result<std::string>::failure(std::move(bytes.error));
    }
    if (bytes.value->empty()) {
        return Result<std::string>::success("");
    }
    return Result<std::string>::success(
        std::string(reinterpret_cast<const char*>(bytes.value->data()), bytes.value->size()));
}

Result<void> ensureDirectory(const std::filesystem::path& path) {
    std::error_code error;
    if (path.empty() || std::filesystem::exists(path, error)) {
        if (error) {
            return Result<void>::failure(ioError("cannot inspect directory", path));
        }
        return Result<void>::success();
    }
    if (!std::filesystem::create_directories(path, error) || error) {
        return Result<void>::failure(ioError("cannot create directory", path));
    }
    return Result<void>::success();
}

Result<void> writeFileAtomic(const std::filesystem::path& path,
                             const std::vector<std::uint8_t>& bytes) {
    auto directory = ensureDirectory(path.parent_path());
    if (!directory.ok()) {
        return directory;
    }
    const std::filesystem::path temporary = path.string() + ".tmp";
    auto write = writeDurable(temporary, bytes);
    if (!write.ok()) {
        return write;
    }
#ifdef _WIN32
    if (!MoveFileExW(temporary.wstring().c_str(), path.wstring().c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        return Result<void>::failure(ioError("cannot atomically replace file", path));
    }
#else
    if (std::rename(pathString(temporary).c_str(), pathString(path).c_str()) != 0) {
        return Result<void>::failure(ioError("cannot atomically replace file", path));
    }
    const std::string directoryPath =
        pathString(path.parent_path().empty() ? std::filesystem::path(".")
                                              : path.parent_path());
    const int directoryFd = ::open(directoryPath.c_str(), O_RDONLY);
    if (directoryFd >= 0) {
        ::fsync(directoryFd);
        ::close(directoryFd);
    }
#endif
    return Result<void>::success();
}

Result<void> writeFileAtomic(const std::filesystem::path& path, const std::string& text) {
    const auto bytes = std::vector<std::uint8_t>(text.begin(), text.end());
    return writeFileAtomic(path, bytes);
}

std::string pathString(const std::filesystem::path& path) {
#ifdef _WIN32
    return path.u8string();
#else
    return path.string();
#endif
}

std::string hexEncode(const std::vector<std::uint8_t>& bytes) {
    static constexpr char digits[] = "0123456789abcdef";
    std::string result;
    result.resize(bytes.size() * 2);
    for (std::size_t i = 0; i < bytes.size(); ++i) {
        result[i * 2] = digits[(bytes[i] >> 4u) & 0x0fu];
        result[i * 2 + 1] = digits[bytes[i] & 0x0fu];
    }
    return result;
}

std::vector<std::uint8_t> bytesOf(const std::string& text) {
    return std::vector<std::uint8_t>(text.begin(), text.end());
}

}  // namespace font_atlas
