#pragma once
#include "vforensics/types.h"
#include <QString>
#include <map>

namespace vforensics {

struct CachedFrame {
  QByteArray expectedHash;
  QByteArray contentHash;
  bool hashMatch = false;
};

struct CachedCascade {
  bool cascadeUntrusted = false;
  std::vector<uint32_t> cascadePath;
};

// Persistent per-frame / per-manifest cache. Storage is JSON written via
// temp-file + atomic rename so a crashed run never corrupts the cache.
class CacheStore {
public:
  explicit CacheStore(QString cacheDir);

  Error load();
  Error save() const;

  // Key includes both the declared checksum and the actual content digest:
  // re-encoded pixels change the key, so stale cached verdicts are never
  // silently reused for tampered content.
  static QString frameKey(const QByteArray& expectedHash,
                          const QByteArray& contentHash, uint32_t index);
  static QString cascadeKey(const QByteArray& manifestHash, uint32_t index);

  const CachedFrame* lookupFrame(const QString& key) const;
  const CachedCascade* lookupCascade(const QString& key) const;
  void putFrame(const QString& key, const CachedFrame& v);
  void putCascade(const QString& key, const CachedCascade& v);

private:
  QString cacheDir_;
  std::map<QString, CachedFrame> frames_;
  std::map<QString, CachedCascade> cascades_;
};

} // namespace vforensics
