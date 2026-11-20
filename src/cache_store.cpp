#include "vforensics/cache_store.h"
#include "vforensics/byte_util.h"
#include <QDir>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QSaveFile>

namespace vforensics {

CacheStore::CacheStore(QString cacheDir) : cacheDir_(std::move(cacheDir)) {}

QString CacheStore::frameKey(const QByteArray& expectedHash,
                             const QByteArray& contentHash, uint32_t index) {
  return QStringLiteral("f:%1:%2:%3")
      .arg(index)
      .arg(QString::fromLatin1(expectedHash.toHex().left(12)))
      .arg(QString::fromLatin1(contentHash.toHex().left(12)));
}

QString CacheStore::cascadeKey(const QByteArray& manifestHash, uint32_t index) {
  return QStringLiteral("c:%1:%2").arg(index).arg(QString::fromLatin1(manifestHash.toHex().left(16)));
}

const CachedFrame* CacheStore::lookupFrame(const QString& key) const {
  auto it = frames_.find(key);
  return it == frames_.end() ? nullptr : &it->second;
}

const CachedCascade* CacheStore::lookupCascade(const QString& key) const {
  auto it = cascades_.find(key);
  return it == cascades_.end() ? nullptr : &it->second;
}

void CacheStore::putFrame(const QString& key, const CachedFrame& v) { frames_[key] = v; }
void CacheStore::putCascade(const QString& key, const CachedCascade& v) { cascades_[key] = v; }

Error CacheStore::load() {
  QFile f(QDir(cacheDir_).filePath("cache.json"));
  if (!f.exists()) return Error::success();
  if (!f.open(QIODevice::ReadOnly)) {
    return Error::make(StatusCode::E_IO, "cannot open cache file");
  }
  const QJsonDocument doc = QJsonDocument::fromJson(f.readAll());
  f.close();
  if (!doc.isObject()) {
    return Error::make(StatusCode::E_RECOVERY_INVALID, "cache JSON malformed");
  }
  const QJsonObject root = doc.object();
  const QJsonObject fr = root.value("frames").toObject();
  for (auto it = fr.begin(); it != fr.end(); ++it) {
    const QJsonObject o = it.value().toObject();
    CachedFrame c;
    c.expectedHash = QByteArray::fromHex(o.value("expected").toString().toLatin1());
    c.contentHash = QByteArray::fromHex(o.value("content").toString().toLatin1());
    c.hashMatch = o.value("match").toBool();
    frames_[it.key()] = c;
  }
  const QJsonObject ca = root.value("cascades").toObject();
  for (auto it = ca.begin(); it != ca.end(); ++it) {
    const QJsonObject o = it.value().toObject();
    CachedCascade c;
    c.cascadeUntrusted = o.value("untrusted").toBool();
    for (const QJsonValue& v : o.value("path").toArray()) c.cascadePath.push_back(v.toInt());
    cascades_[it.key()] = c;
  }
  return Error::success();
}

Error CacheStore::save() const {
  QDir().mkpath(cacheDir_);
  QJsonObject fr;
  for (const auto& kv : frames_) {
    QJsonObject o;
    o["expected"] = QString::fromLatin1(kv.second.expectedHash.toHex());
    o["content"] = QString::fromLatin1(kv.second.contentHash.toHex());
    o["match"] = kv.second.hashMatch;
    fr[kv.first] = o;
  }
  QJsonObject ca;
  for (const auto& kv : cascades_) {
    QJsonObject o;
    o["untrusted"] = kv.second.cascadeUntrusted;
    QJsonArray path;
    for (uint32_t x : kv.second.cascadePath) path.append(static_cast<int>(x));
    o["path"] = path;
    ca[kv.first] = o;
  }
  QJsonObject root;
  root["frames"] = fr;
  root["cascades"] = ca;
  const QString path = QDir(cacheDir_).filePath("cache.json");
  QSaveFile sf(path);
  if (!sf.open(QIODevice::WriteOnly)) {
    return Error::make(StatusCode::E_IO, "cannot write cache file");
  }
  sf.write(QJsonDocument(root).toJson(QJsonDocument::Compact));
  if (!sf.commit()) {
    return Error::make(StatusCode::E_IO, "atomic cache commit failed");
  }
  return Error::success();
}

} // namespace vforensics
