#include "vforensics/types.h"
#include <QJsonArray>

namespace vforensics {

QString statusName(StatusCode code) {
  switch (code) {
    case StatusCode::Ok: return "OK";
    case StatusCode::E_FRAME_INVALID: return "E_FRAME_INVALID";
    case StatusCode::E_DEP_CYCLE: return "E_DEP_CYCLE";
    case StatusCode::E_DEP_DANGLING: return "E_DEP_DANGLING";
    case StatusCode::E_MANIFEST_VERSION: return "E_MANIFEST_VERSION";
    case StatusCode::E_RECOVERY_INVALID: return "E_RECOVERY_INVALID";
    case StatusCode::E_BUSY: return "E_BUSY";
    case StatusCode::E_REPORT_INVALID: return "E_REPORT_INVALID";
    case StatusCode::E_IO: return "E_IO";
    case StatusCode::E_INTERNAL: return "E_INTERNAL";
  }
  return "E_INTERNAL";
}

bool ForensicResult::frameFailed(uint32_t index) const {
  return index < frames.size() && !frames[index].hashMatch;
}

FrameTrust ForensicResult::trustOf(uint32_t index) const {
  if (index >= frames.size()) return FrameTrust::CascadedUntrusted;
  if (!frames[index].hashMatch) return FrameTrust::HashMismatch;
  if (frames[index].cascadeUntrusted) return FrameTrust::CascadedUntrusted;
  return FrameTrust::Trusted;
}

QJsonObject ForensicResult::toJson() const {
  QJsonObject root;
  root["resultVersion"] = QString::number(resultVersion);
  root["publishedAtMs"] = QString::number(publishedAtMs);
  root["manifestVersion"] = static_cast<int>(manifest.manifestVersion);
  root["inputFileHash"] = QString::fromLatin1(inputFileHash.toHex());
  QJsonObject seq;
  seq["width"] = static_cast<int>(sequence.width);
  seq["height"] = static_cast<int>(sequence.height);
  seq["channels"] = static_cast<int>(sequence.channels);
  seq["frameCount"] = static_cast<int>(sequence.frameCount);
  root["sequence"] = seq;
  return root;
}

} // namespace vforensics
