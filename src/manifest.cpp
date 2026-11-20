#include "vforensics/manifest.h"
#include <QCryptographicHash>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>

namespace vforensics {

namespace {

QString requireString(const QJsonObject& o, const char* key, bool& ok) {
  const QJsonValue v = o.value(QLatin1String(key));
  if (!v.isString() || v.toString().isEmpty()) ok = false;
  return v.toString();
}
uint32_t requireUint(const QJsonObject& o, const char* key, bool& ok) {
  const QJsonValue v = o.value(QLatin1String(key));
  if (!v.isDouble()) { ok = false; return 0; }
  return static_cast<uint32_t>(v.toDouble());
}

Error parseAnchor(const QJsonObject& o, uint32_t manifestVersion, AnchorRecord& a) {
  bool ok = true;
  a.segmentId = requireUint(o, "segmentId", ok);
  a.startFrame = requireUint(o, "startFrame", ok);
  a.endFrame = requireUint(o, "endFrame", ok);
  a.watermarkX = requireUint(o, "watermarkX", ok);
  a.watermarkY = requireUint(o, "watermarkY", ok);
  a.referenceType = requireString(o, "referenceType", ok);
  // Version 2 made watermark position and reference type mandatory semantics.
  // A manifest that omits them cannot be silently migrated from v1 defaults.
  if (!ok || a.endFrame < a.startFrame) {
    return Error::make(StatusCode::E_MANIFEST_VERSION,
                       QStringLiteral("anchor segment %1 missing/invalid semantic fields (manifest v%2)")
                           .arg(a.segmentId).arg(manifestVersion));
  }
  return Error::success();
}

} // namespace

std::vector<AnchorDiff> Manifest::diffAnchors(const ManifestData& oldM, const ManifestData& newM) {
  std::vector<AnchorDiff> out;
  for (const AnchorRecord& a : newM.anchors) {
    const auto it = std::find_if(oldM.anchors.begin(), oldM.anchors.end(),
                                 [&](const AnchorRecord& b) { return b.segmentId == a.segmentId; });
    if (it == oldM.anchors.end()) {
      out.push_back({a.segmentId, "ADDED",
                     QStringLiteral("segment %1 added in v%2").arg(a.segmentId).arg(newM.manifestVersion)});
      continue;
    }
    if (it->startFrame != a.startFrame || it->endFrame != a.endFrame) {
      out.push_back({a.segmentId, "RANGE_CHANGED",
                     QStringLiteral("[%1-%2] -> [%3-%4]")
                         .arg(it->startFrame).arg(it->endFrame).arg(a.startFrame).arg(a.endFrame)});
    }
    if (it->watermarkX != a.watermarkX || it->watermarkY != a.watermarkY) {
      out.push_back({a.segmentId, "WATERMARK_CHANGED",
                     QStringLiteral("watermark (%1,%2) -> (%3,%4)")
                         .arg(it->watermarkX).arg(it->watermarkY).arg(a.watermarkX).arg(a.watermarkY)});
    }
    if (it->referenceType != a.referenceType) {
      out.push_back({a.segmentId, "REFERENCE_TYPE_CHANGED",
                     QStringLiteral("%1 -> %2").arg(it->referenceType, a.referenceType)});
    }
  }
  for (const AnchorRecord& a : oldM.anchors) {
    const bool stillExists = std::any_of(newM.anchors.begin(), newM.anchors.end(),
                                         [&](const AnchorRecord& b) { return b.segmentId == a.segmentId; });
    if (!stillExists) {
      out.push_back({a.segmentId, "REMOVED",
                     QStringLiteral("segment %1 removed in v%2").arg(a.segmentId).arg(newM.manifestVersion)});
    }
  }
  std::sort(out.begin(), out.end(), [](const AnchorDiff& x, const AnchorDiff& y) {
    return x.segmentId != y.segmentId ? x.segmentId < y.segmentId : x.change < y.change;
  });
  return out;
}

Error Manifest::parse(const QString& path,
                      std::optional<uint32_t> expectedFrameCount,
                      ManifestData& out,
                      std::vector<AnchorDiff>* diffs,
                      const ManifestData* previous) {
  QFile f(path);
  if (!f.open(QIODevice::ReadOnly)) {
    return Error::make(StatusCode::E_IO, QStringLiteral("cannot open manifest: %1").arg(path));
  }
  const QByteArray raw = f.readAll();
  f.close();

  QJsonParseError perr;
  const QJsonDocument doc = QJsonDocument::fromJson(raw, &perr);
  if (perr.error != QJsonParseError::NoError || !doc.isObject()) {
    return Error::make(StatusCode::E_FRAME_INVALID,
                       QStringLiteral("manifest JSON invalid: %1").arg(perr.errorString()));
  }
  const QJsonObject root = doc.object();

  const QJsonValue vv = root.value("manifestVersion");
  if (!vv.isDouble()) {
    return Error::make(StatusCode::E_MANIFEST_VERSION, "manifestVersion field missing");
  }
  const uint32_t version = static_cast<uint32_t>(vv.toDouble());
  if (version != 1 && version != 2) {
    return Error::make(StatusCode::E_MANIFEST_VERSION,
                       QStringLiteral("unsupported manifest version %1").arg(version));
  }

  const QJsonValue edges = root.value("dependencies");
  if (!edges.isArray()) {
    return Error::make(StatusCode::E_MANIFEST_VERSION, "dependencies array missing");
  }
  const QJsonArray edgeArr = edges.toArray();
  if (edgeArr.isEmpty()) {
    return Error::make(StatusCode::E_MANIFEST_VERSION, "dependencies array is empty");
  }

  // Version-upgrade guard: v2 must restate every anchor with explicit
  // watermark/reference semantics; implicit reuse of v1 anchors is rejected.
  const QJsonValue anchorsVal = root.value("anchors");
  if (version == 2 && (!anchorsVal.isArray() || anchorsVal.toArray().isEmpty())) {
    return Error::make(StatusCode::E_MANIFEST_VERSION,
                       "manifest upgraded to v2 without explicit anchor semantics; refusing implicit migration");
  }

  ManifestData m;
  m.manifestVersion = version;
  m.sourcePath = path;
  m.rawHash = QCryptographicHash::hash(raw, QCryptographicHash::Sha256);

  uint32_t maxIndex = 0;
  struct Edge { uint32_t from; uint32_t to; };
  std::vector<Edge> parsedEdges;
  for (const QJsonValue& ev : edgeArr) {
    if (!ev.isObject()) {
      return Error::make(StatusCode::E_MANIFEST_VERSION, "dependency entry is not an object");
    }
    const QJsonObject eo = ev.toObject();
    bool ok1 = true;
    const uint32_t frame = requireUint(eo, "frame", ok1);
    const QJsonValue refs = eo.value("references");
    if (!ok1 || !refs.isArray()) {
      return Error::make(StatusCode::E_MANIFEST_VERSION,
                         QStringLiteral("frame %1 dependency malformed").arg(frame));
    }
    std::vector<uint32_t> refList;
    for (const QJsonValue& rv : refs.toArray()) {
      if (!rv.isDouble()) {
        return Error::make(StatusCode::E_MANIFEST_VERSION, "reference id is not numeric");
      }
      const uint32_t r = static_cast<uint32_t>(rv.toDouble());
      refList.push_back(r);
      parsedEdges.push_back({frame, r});
    }
    maxIndex = std::max(maxIndex, frame);
    for (uint32_t r : refList) maxIndex = std::max(maxIndex, r);
    if (m.references.size() <= frame) m.references.resize(frame + 1);
    m.references[frame] = std::move(refList);
  }

  const uint32_t graphSize = expectedFrameCount.value_or(maxIndex + 1);
  if (expectedFrameCount && maxIndex >= *expectedFrameCount) {
    return Error::make(StatusCode::E_DEP_DANGLING,
                       QStringLiteral("dependency references frame %1 outside frame count %2")
                           .arg(maxIndex).arg(*expectedFrameCount));
  }
  m.references.resize(graphSize);

  if (anchorsVal.isArray()) {
    for (const QJsonValue& av : anchorsVal.toArray()) {
      if (!av.isObject()) {
        return Error::make(StatusCode::E_MANIFEST_VERSION, "anchor entry is not an object");
      }
      AnchorRecord a;
      Error e = parseAnchor(av.toObject(), version, a);
      if (!e.ok()) return e;
      if (a.endFrame >= graphSize) {
        return Error::make(StatusCode::E_DEP_DANGLING,
                           QStringLiteral("anchor %1 extends beyond frame count").arg(a.segmentId));
      }
      m.anchors.push_back(a);
    }
  }
  if (previous && previous->manifestVersion != version) {
    // Older manifest being reused implicitly after upgrade is forbidden:
    // require full anchor restatement before producing a diff summary.
    if (version > previous->manifestVersion && m.anchors.empty()) {
      return Error::make(StatusCode::E_MANIFEST_VERSION,
                         "upgraded manifest carries no anchors; cannot infer from older version");
    }
    if (diffs) *diffs = diffAnchors(*previous, m);
  }

  out = std::move(m);
  return Error::success();
}

} // namespace vforensics
