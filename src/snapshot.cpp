#include "vforensics/snapshot.h"
#include <QCryptographicHash>
#include <QDir>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QSaveFile>

namespace vforensics {

namespace {

QJsonArray uintVec(const std::vector<uint32_t>& v) {
  QJsonArray a;
  for (uint32_t x : v) a.append(static_cast<int>(x));
  return a;
}

QJsonObject frameToJson(const FrameVerification& fv) {
  QJsonObject o;
  o["index"] = static_cast<int>(fv.index);
  o["timestampUs"] = QString::number(fv.timestampUs);
  o["contentHash"] = QString::fromLatin1(fv.contentHash.toHex());
  o["hashMatch"] = fv.hashMatch;
  o["firstMismatchOffset"] = static_cast<qint64>(fv.firstMismatchOffset);
  o["firstMismatchAbsolute"] = static_cast<qint64>(fv.firstMismatchAbsolute);
  o["cascadeUntrusted"] = fv.cascadeUntrusted;
  o["cascadePath"] = uintVec(fv.cascadePath);
  return o;
}

bool frameFromJson(const QJsonObject& o, FrameVerification& fv, QString& why) {
  const QStringList required = {"index", "timestampUs", "contentHash", "hashMatch",
                                "firstMismatchOffset", "firstMismatchAbsolute",
                                "cascadeUntrusted", "cascadePath"};
  for (const QString& k : required) {
    if (!o.contains(k)) { why = QStringLiteral("frame field missing: %1").arg(k); return false; }
  }
  fv.index = o.value("index").toInt();
  fv.timestampUs = o.value("timestampUs").toString().toULongLong();
  fv.contentHash = QByteArray::fromHex(o.value("contentHash").toString().toLatin1());
  fv.hashMatch = o.value("hashMatch").toBool();
  fv.firstMismatchOffset = static_cast<int64_t>(o.value("firstMismatchOffset").toVariant().toLongLong());
  fv.firstMismatchAbsolute = static_cast<int64_t>(o.value("firstMismatchAbsolute").toVariant().toLongLong());
  fv.cascadeUntrusted = o.value("cascadeUntrusted").toBool();
  for (const QJsonValue& v : o.value("cascadePath").toArray()) fv.cascadePath.push_back(v.toInt());
  return true;
}

QJsonObject anchorDiffToJson(const AnchorDiff& d) {
  QJsonObject o;
  o["segmentId"] = static_cast<int>(d.segmentId);
  o["change"] = d.change;
  o["detail"] = d.detail;
  return o;
}

} // namespace

SnapshotStore::SnapshotStore(QString workdir) : workdir_(std::move(workdir)) {}

QString SnapshotStore::dir() const { return QDir(workdir_).filePath("snapshots"); }

QByteArray SnapshotStore::computeRecordHash(const QByteArray& prevHash, const QJsonObject& body) {
  QCryptographicHash h(QCryptographicHash::Sha256);
  h.addData(prevHash.toHex());
  h.addData(QJsonDocument(body).toJson(QJsonDocument::Compact));
  return h.result();
}

Error SnapshotStore::publish(const ForensicResult& result, const QByteArray& prevRecordHash,
                             QByteArray& newRecordHash, QString& publishedPath) const {
  QDir().mkpath(dir());

  QJsonObject body;
  QJsonObject seq;
  seq["version"] = static_cast<int>(result.sequence.version);
  seq["width"] = static_cast<int>(result.sequence.width);
  seq["height"] = static_cast<int>(result.sequence.height);
  seq["channels"] = static_cast<int>(result.sequence.channels);
  seq["frameCount"] = static_cast<int>(result.sequence.frameCount);
  seq["baseTimestampUs"] = QString::number(result.sequence.baseTimestampUs);
  seq["framePeriodUs"] = QString::number(result.sequence.framePeriodUs);
  body["sequence"] = seq;

  QJsonObject man;
  man["manifestVersion"] = static_cast<int>(result.manifest.manifestVersion);
  man["manifestHash"] = QString::fromLatin1(result.manifest.rawHash.toHex());
  QJsonArray deps;
  for (uint32_t i = 0; i < result.manifest.references.size(); ++i) {
    QJsonObject e;
    e["frame"] = static_cast<int>(i);
    e["references"] = uintVec(result.manifest.references[i]);
    deps.append(e);
  }
  man["dependencies"] = deps;
  QJsonArray anchors;
  for (const AnchorRecord& a : result.manifest.anchors) {
    QJsonObject o;
    o["segmentId"] = static_cast<int>(a.segmentId);
    o["startFrame"] = static_cast<int>(a.startFrame);
    o["endFrame"] = static_cast<int>(a.endFrame);
    o["watermarkX"] = static_cast<int>(a.watermarkX);
    o["watermarkY"] = static_cast<int>(a.watermarkY);
    o["referenceType"] = a.referenceType;
    anchors.append(o);
  }
  man["anchors"] = anchors;
  body["manifest"] = man;

  QJsonArray frames;
  for (const FrameVerification& fv : result.frames) frames.append(frameToJson(fv));
  body["frames"] = frames;
  QJsonArray diffs;
  for (const AnchorDiff& d : result.anchorDiffs) diffs.append(anchorDiffToJson(d));
  body["anchorDiffs"] = diffs;
  body["resultVersion"] = QString::number(result.resultVersion);
  body["publishedAtMs"] = QString::number(result.publishedAtMs);
  body["inputFileHash"] = QString::fromLatin1(result.inputFileHash.toHex());
  body["snapshotFormatVersion"] = 1;

  newRecordHash = computeRecordHash(prevRecordHash, body);
  QJsonObject envelope;
  envelope["prevRecordHash"] = QString::fromLatin1(prevRecordHash.toHex());
  envelope["recordHash"] = QString::fromLatin1(newRecordHash.toHex());
  envelope["body"] = body;

  const QString base = QStringLiteral("snapshot.v%1.json").arg(result.resultVersion);
  const QString finalPath = QDir(dir()).filePath(base);
  // Temp file + atomic rename: readers keep seeing the old version until
  // the new snapshot is fully durable.
  QSaveFile sf(finalPath);
  if (!sf.open(QIODevice::WriteOnly)) {
    return Error::make(StatusCode::E_IO, "cannot open snapshot temp file");
  }
  sf.write(QJsonDocument(envelope).toJson(QJsonDocument::Indented));
  if (!sf.commit()) {
    return Error::make(StatusCode::E_IO, "snapshot atomic rename failed");
  }

  QSaveFile latest(QDir(dir()).filePath("latest.json"));
  if (!latest.open(QIODevice::WriteOnly)) {
    return Error::make(StatusCode::E_IO, "cannot open latest pointer temp file");
  }
  latest.write(base.toUtf8());
  if (!latest.commit()) {
    return Error::make(StatusCode::E_IO, "latest pointer atomic rename failed");
  }
  publishedPath = finalPath;
  return Error::success();
}

Error SnapshotStore::loadLatest(ForensicResult& out, QByteArray& recordHash, uint64_t& version) const {
  const QString snapDir = dir();
  QFile pointer(QDir(snapDir).filePath("latest.json"));
  if (!pointer.exists()) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "no published snapshot exists");
  }
  if (!pointer.open(QIODevice::ReadOnly)) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "latest pointer unreadable");
  }
  const QString name = QString::fromUtf8(pointer.readAll()).trimmed();
  pointer.close();
  const QString path = QDir(snapDir).filePath(name);
  QFile f(path);
  if (!f.open(QIODevice::ReadOnly)) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "snapshot file missing");
  }
  const QJsonDocument doc = QJsonDocument::fromJson(f.readAll());
  f.close();
  if (!doc.isObject()) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "snapshot JSON malformed");
  }
  const QJsonObject envelope = doc.object();
  if (!envelope.contains("prevRecordHash") || !envelope.contains("recordHash") ||
      !envelope.contains("body")) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "snapshot envelope fields missing");
  }
  const QJsonObject body = envelope.value("body").toObject();
  const QByteArray prevHash = QByteArray::fromHex(envelope.value("prevRecordHash").toString().toLatin1());
  const QByteArray wantHash = QByteArray::fromHex(envelope.value("recordHash").toString().toLatin1());
  const QByteArray haveHash = computeRecordHash(prevHash, body);
  if (haveHash != wantHash) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "snapshot hash chain broken");
  }

  ForensicResult r;
  const QJsonObject seq = body.value("sequence").toObject();
  const QStringList seqReq = {"version", "width", "height", "channels", "frameCount",
                              "baseTimestampUs", "framePeriodUs"};
  for (const QString& k : seqReq) {
    if (!seq.contains(k)) return Error::make(StatusCode::E_RECOVERY_INVALID,
                                             QStringLiteral("sequence field missing: %1").arg(k));
  }
  r.sequence.version = seq.value("version").toInt();
  r.sequence.width = seq.value("width").toInt();
  r.sequence.height = seq.value("height").toInt();
  r.sequence.channels = seq.value("channels").toInt();
  r.sequence.frameCount = seq.value("frameCount").toInt();
  r.sequence.baseTimestampUs = seq.value("baseTimestampUs").toString().toULongLong();
  r.sequence.framePeriodUs = seq.value("framePeriodUs").toString().toULongLong();

  const QJsonObject man = body.value("manifest").toObject();
  if (!man.contains("manifestVersion") || !man.contains("manifestHash") ||
      !man.contains("dependencies") || !man.contains("anchors")) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "manifest section fields missing");
  }
  r.manifest.manifestVersion = man.value("manifestVersion").toInt();
  r.manifest.rawHash = QByteArray::fromHex(man.value("manifestHash").toString().toLatin1());
  r.manifest.references.resize(r.sequence.frameCount);
  for (const QJsonValue& ev : man.value("dependencies").toArray()) {
    const QJsonObject eo = ev.toObject();
    const uint32_t idx = eo.value("frame").toInt();
    if (idx >= r.sequence.frameCount) {
      return Error::make(StatusCode::E_RECOVERY_INVALID, "dependency index out of range");
    }
    for (const QJsonValue& rv : eo.value("references").toArray()) {
      r.manifest.references[idx].push_back(rv.toInt());
    }
  }
  for (const QJsonValue& av : man.value("anchors").toArray()) {
    const QJsonObject ao = av.toObject();
    AnchorRecord a;
    a.segmentId = ao.value("segmentId").toInt();
    a.startFrame = ao.value("startFrame").toInt();
    a.endFrame = ao.value("endFrame").toInt();
    a.watermarkX = ao.value("watermarkX").toInt();
    a.watermarkY = ao.value("watermarkY").toInt();
    a.referenceType = ao.value("referenceType").toString();
    r.manifest.anchors.push_back(a);
  }

  if (!body.contains("frames") || !body.contains("anchorDiffs") ||
      !body.contains("resultVersion") || !body.contains("publishedAtMs") ||
      !body.contains("inputFileHash")) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "top-level snapshot fields missing");
  }
  for (const QJsonValue& fv : body.value("frames").toArray()) {
    FrameVerification f;
    QString why;
    if (!frameFromJson(fv.toObject(), f, why)) {
      return Error::make(StatusCode::E_RECOVERY_INVALID, why);
    }
    r.frames.push_back(f);
  }
  if (r.frames.size() != r.sequence.frameCount) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "snapshot frame count mismatch");
  }
  for (const QJsonValue& dv : body.value("anchorDiffs").toArray()) {
    const QJsonObject d = dv.toObject();
    r.anchorDiffs.push_back({static_cast<uint32_t>(d.value("segmentId").toInt()),
                             d.value("change").toString(), d.value("detail").toString()});
  }
  r.resultVersion = body.value("resultVersion").toString().toULongLong();
  r.publishedAtMs = body.value("publishedAtMs").toString().toULongLong();
  r.inputFileHash = QByteArray::fromHex(body.value("inputFileHash").toString().toLatin1());

  out = std::move(r);
  recordHash = wantHash;
  version = out.resultVersion;
  return Error::success();
}

} // namespace vforensics
