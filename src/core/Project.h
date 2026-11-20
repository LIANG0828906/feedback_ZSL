#pragma once

#include "core/Deviation.h"
#include "core/Editing.h"

#include <cstdint>
#include <string>
#include <vector>

namespace meshedit {

struct ProjectData {
    Mesh current;
    Mesh baseline;
    std::vector<EditRecord> undoHistory;
    std::vector<EditRecord> redoHistory;
    int exportVersion = 2;
    double weldTolerance = 1e-6;
    ValidationConfig validation;
};

class ProjectIo {
public:
    static constexpr std::uint32_t kProjectVersion = 1;

    static Result<bool> saveAtomic(const std::string& path, const ProjectData& project);
    static Result<ProjectData> load(const std::string& path);
    static std::uint64_t contentHash(const std::vector<unsigned char>& payload);
};

} // namespace meshedit
