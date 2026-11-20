#pragma once
#include "vforensics/types.h"
#include <QString>

namespace vforensics {

// Snapshot files live in <workdir>/snapshots/:
//   snapshot.v<N>.tmp  (in-progress, never read as a published result)
//   snapshot.v<N>.json (atomically renamed published version)
//   latest.json        (atomically renamed pointer to the newest version)
//
// Each snapshot carries a hash chain: recordHashN =
// SHA256(prevRecordHash || canonicalSnapshotBodyWithoutHash).
class SnapshotStore {
public:
  explicit SnapshotStore(QString workdir);

  Error publish(const ForensicResult& result, const QByteArray& prevRecordHash,
                QByteArray& newRecordHash, QString& publishedPath) const;

  // Loads and verifies the newest valid snapshot. A broken chain or missing
  // field returns E_RECOVERY_INVALID and never overwrites existing snapshots.
  Error loadLatest(ForensicResult& out, QByteArray& recordHash, uint64_t& version) const;

  QString dir() const;
  static QByteArray computeRecordHash(const QByteArray& prevHash, const QJsonObject& body);

private:
  QString workdir_;
};

} // namespace vforensics
