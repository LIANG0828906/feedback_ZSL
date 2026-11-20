#include "core/VersionCompat.h"

namespace meshedit {

Result<ConversionReport> upgradeLoadedMesh(Mesh& mesh, MeshFormat format, int sourceVersion) {
    ConversionReport report;
    report.sourceFormat = format;
    report.targetFormat = format;
    report.sourceVersion = sourceVersion;
    report.targetVersion = 2;
    if (sourceVersion <= 0 || sourceVersion > 2) {
        Error error;
        error.code = Code::VersionLoss;
        error.message = "file version is outside the supported v1/v2 range";
        return Result<ConversionReport>(std::move(error));
    }
    if (sourceVersion == 1) {
        // v1 stores one UV/normal pair per shared position. The internal model
        // already uses per-corner indices, so preserving each corner is a strict
        // superset of the old rule and no geometry needs to be modified.
        mesh.sourceVersion = 2;
        report.upgradedOnLoad = true;
        report.notes.push_back(
            "v1 upgraded in memory to v2: per-corner UV/normal indices enabled");
    } else {
        mesh.sourceVersion = 2;
        report.notes.push_back("v2 loaded without compatibility transforms");
    }
    return Result<ConversionReport>(std::move(report));
}

Result<ConversionReport> checkExportCompatibility(const Mesh& mesh,
                                                  const ExportOptions& options) {
    if (options.format == MeshFormat::Obj) {
        return checkObjVersionLoss(mesh, options.version);
    }
    ConversionReport report;
    report.sourceFormat = MeshFormat::Obj;
    report.targetFormat = MeshFormat::Stl;
    report.sourceVersion = 2;
    report.targetVersion = 2;
    report.notes.push_back("STL has no versioned group/share rule; byte variant controls encoding");
    return Result<ConversionReport>(std::move(report));
}

} // namespace meshedit
