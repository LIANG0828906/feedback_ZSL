#pragma once

#include "core/Error.h"

#include <string>

namespace meshedit {

struct SampleManifest {
    std::string directory;
    int fileCount = 0;
    long long totalBytes = 0;
    std::string toJson() const;
};

Result<SampleManifest> generateDeterministicSamples(const std::string& outputDirectory);

} // namespace meshedit
