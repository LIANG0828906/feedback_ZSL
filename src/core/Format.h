#pragma once

#include "core/Error.h"
#include "core/Mesh.h"

#include <string>
#include <vector>

namespace meshedit {

enum class MeshFormat { Obj, Stl };
enum class StlVariant { Auto, Ascii, Binary };
enum class Version { V1 = 1, V2 = 2 };

struct LossItem {
    Code code = Code::VersionLoss;
    std::string message;
    std::vector<std::string> affectedGroups;
    int line = 0;
};

struct ConversionReport {
    std::vector<LossItem> losses;
    std::vector<std::string> notes;
    int sourceVersion = 2;
    int targetVersion = 2;
    MeshFormat sourceFormat = MeshFormat::Obj;
    MeshFormat targetFormat = MeshFormat::Obj;
    bool upgradedOnLoad = false;

    bool hasLoss() const { return !losses.empty(); }
    std::string toText() const;
    std::string toJson() const;
};

struct ExportOptions {
    MeshFormat format = MeshFormat::Obj;
    Version version = Version::V2;
    StlVariant stlVariant = StlVariant::Binary;
    bool recomputeNormals = false;
};

struct ExportResult {
    ConversionReport report;
    std::size_t bytesWritten = 0;
};

struct LoadOptions {
    StlVariant stlVariant = StlVariant::Auto;
};

struct LoadResult {
    Mesh mesh;
    ConversionReport report;
};

MeshFormat formatFromPath(const std::string& path);
const char* formatName(MeshFormat format);
int versionNumber(Version version);

Result<LoadResult> loadMesh(const std::string& path, const LoadOptions& options = {});
Result<ExportResult> exportMesh(const Mesh& mesh, const std::string& path,
                                const ExportOptions& options);

Result<LoadResult> parseObj(const std::string& path);
Result<ExportResult> writeObj(const Mesh& mesh, const std::string& path,
                              Version version);
Result<LoadResult> parseStl(const std::string& path, StlVariant variant = StlVariant::Auto);
Result<ExportResult> writeStl(const Mesh& mesh, const std::string& path,
                              StlVariant variant);

Result<ConversionReport> checkObjVersionLoss(const Mesh& mesh, Version target);

} // namespace meshedit
