#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "json.h"

namespace caisson {

struct StructuredError {
    std::string code;
    std::string message;
    int layerId = -1;
    int quadrant = -1;
    std::string objectId;

    explicit StructuredError(std::string code_value = "E_UNKNOWN")
        : code(std::move(code_value)) {}

    std::string toJson() const;
};

bool readTextFile(const std::filesystem::path& path, std::string& output, std::string& error);
bool readBinaryFile(
    const std::filesystem::path& path,
    std::vector<std::uint8_t>& output,
    std::string& error);
bool writeBinaryFile(
    const std::filesystem::path& path,
    const std::vector<std::uint8_t>& bytes,
    std::string& error);
bool atomicWriteText(
    const std::filesystem::path& path,
    const std::string& text,
    std::string& error);
bool atomicWriteBinary(
    const std::filesystem::path& path,
    const std::vector<std::uint8_t>& bytes,
    std::string& error);
bool replaceFile(
    const std::filesystem::path& temporary,
    const std::filesystem::path& destination,
    std::string& error);
bool ensureDirectory(const std::filesystem::path& path, std::string& error);

std::string formatDouble(double value);
std::string formatFixed(double value, int precision);
std::string pathForReport(const std::filesystem::path& relative);
std::string quote(const std::string& value);

std::uint64_t fnv1a64(const void* data, std::size_t size);
std::uint64_t fnv1a64(const std::string& text);

}  // namespace caisson

