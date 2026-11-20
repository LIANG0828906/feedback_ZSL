#include "vforensics/report.h"
#include <QDir>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QSaveFile>
#include <QTextStream>

namespace vforensics {

Error DiagnosticReport::exportJson(const ForensicResult& result, const QString& outPath) {
  if (result.frames.size() != result.sequence.frameCount) {
    return Error::make(StatusCode::E_REPORT_INVALID, "frame list incomplete, refusing export");
  }
  if (result.manifest.manifestVersion == 0) {
    return Error::make(StatusCode::E_REPORT_INVALID, "manifest version missing");
  }

  QJsonArray frameReports;
  for (const FrameVerification& fv : result.frames) {
    if (fv.contentHash.isEmpty()) {
      return Error::make(StatusCode::E_REPORT_INVALID,
                         QStringLiteral("frame %1 content hash missing").arg(fv.index));
    }
    QJsonObject o;
    o["frame"] = static_cast<int>(fv.index);
    o["timestampUs"] = QString::number(fv.timestampUs);
    o["contentHash"] = QString::fromLatin1(fv.contentHash.toHex());
    o["hashVerdict"] = fv.hashMatch ? "MATCH" : "MISMATCH";
    o["evidenceByteOffset"] = static_cast<qint64>(fv.firstMismatchOffset);
    o["evidenceByteAbsolute"] = static_cast<qint64>(fv.firstMismatchAbsolute);
    if (!fv.hashMatch && fv.firstMismatchOffset < 0) {
      return Error::make(StatusCode::E_REPORT_INVALID,
                         QStringLiteral("frame %1 mismatch lacks evidence byte offset").arg(fv.index));
    }
    o["cascadeState"] = fv.cascadeUntrusted ? "UNTRUSTED" : "TRUSTED";
    QJsonArray path;
    for (uint32_t p : fv.cascadePath) path.append(static_cast<int>(p));
    o["cascadePath"] = path;
    if (fv.cascadeUntrusted && fv.cascadePath.empty()) {
      return Error::make(StatusCode::E_REPORT_INVALID,
                         QStringLiteral("frame %1 untrusted without cascade path").arg(fv.index));
    }
    frameReports.append(o);
  }

  QJsonArray diffs;
  for (const AnchorDiff& d : result.anchorDiffs) {
    QJsonObject o;
    o["segmentId"] = static_cast<int>(d.segmentId);
    o["change"] = d.change;
    o["detail"] = d.detail;
    diffs.append(o);
  }

  QJsonObject root;
  root["manifestVersion"] = static_cast<int>(result.manifest.manifestVersion);
  root["manifestHash"] = QString::fromLatin1(result.manifest.rawHash.toHex());
  root["resultVersion"] = QString::number(result.resultVersion);
  root["inputFileHash"] = QString::fromLatin1(result.inputFileHash.toHex());
  root["frames"] = frameReports;
  root["anchorDiffs"] = diffs;

  QDir().mkpath(QFileInfo(outPath).absolutePath());
  QSaveFile sf(outPath);
  if (!sf.open(QIODevice::WriteOnly)) {
    return Error::make(StatusCode::E_IO, "cannot open report output");
  }
  sf.write(QJsonDocument(root).toJson(QJsonDocument::Indented));
  if (!sf.commit()) {
    return Error::make(StatusCode::E_IO, "report atomic write failed");
  }
  return Error::success();
}

QByteArray DiagnosticReport::trajectoryLog(const ForensicResult& result) {
  QByteArray out;
  QTextStream ts(&out);
  for (const FrameVerification& fv : result.frames) {
    ts << "frame=" << fv.index
       << " verdict=" << (fv.hashMatch ? "MATCH" : "MISMATCH")
       << " cascade=" << (fv.cascadeUntrusted ? "UNTRUSTED" : "TRUSTED")
       << " manifest=v" << result.manifest.manifestVersion
       << " sha256=" << QString::fromLatin1(fv.contentHash.toHex())
       << '\n';
  }
  ts.flush();
  return out;
}

} // namespace vforensics
