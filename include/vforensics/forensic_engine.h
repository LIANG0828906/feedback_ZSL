#pragma once
#include "vforensics/types.h"
#include "vforensics/cache_store.h"
#include "vforensics/snapshot.h"
#include <QString>
#include <functional>
#include <atomic>

namespace vforensics {

struct RunStats {
  uint32_t recomputedFrames = 0;
  uint32_t reusedFrames = 0;
  uint32_t affectedFrames = 0;
  uint64_t resultVersion = 0;
  QString publishedSnapshot;
};

class ForensicEngine {
public:
  ForensicEngine(QString workdir, uint64_t fixedClockMs = 0);

  using ProgressCb = std::function<void(uint32_t done, uint32_t total)>;

  // Full/incremental verification. referencePath is optional: when a .vref
  // sidecar exists it supplies original pixels for first-mismatch offsets.
  Error verify(const QString& sequencePath,
               const QString& manifestPath,
               const QString& referencePath,
               RunStats& stats,
               ForensicResult& result,
               const ProgressCb& progress = {},
               const std::atomic<bool>* cancelToken = nullptr);

  // Resume from the newest valid snapshot, then verify only frames that were
  // not completed. Broken chains fail with E_RECOVERY_INVALID.
  Error resume(const QString& sequencePath,
               const QString& manifestPath,
               const QString& referencePath,
               RunStats& stats,
               ForensicResult& result);

  SnapshotStore& snapshots() { return snapshotStore_; }

private:
  Error runInternal(const QString& sequencePath, const QString& manifestPath,
                    const QString& referencePath, bool resumeMode,
                    RunStats& stats, ForensicResult& result, const ProgressCb& progress,
                    const std::atomic<bool>* cancelToken);

  QString workdir_;
  uint64_t fixedClockMs_;
  CacheStore cache_;
  SnapshotStore snapshotStore_;
};

} // namespace vforensics
