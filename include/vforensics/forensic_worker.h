#pragma once
#include "vforensics/forensic_engine.h"
#include <QObject>
#include <QFuture>
#include <QMutex>
#include <atomic>
#include <memory>

namespace vforensics {

// Background verification coordinator.
//  * at most one task per sequence at a time; duplicate submit -> E_BUSY
//  * UI reads only the last PUBLISHED snapshot, never in-progress state
//  * cancel() closes task-owned file handles and preserves published results
class ForensicWorker : public QObject {
  Q_OBJECT
public:
  explicit ForensicWorker(QString workdir, uint64_t fixedClockMs = 0, QObject* parent = nullptr);
  ~ForensicWorker() override;

  struct JobRequest {
    QString sequencePath;
    QString manifestPath;
    QString referencePath;
    bool resume = false;
  };

  Error submit(const JobRequest& req);
  Error cancel();
  bool isBusy() const;

  // Reads the newest published snapshot for UI preview (never partial state).
  Error readPublished(ForensicResult& out) const;

signals:
  void progress(uint32_t done, uint32_t total);
  void finished(vforensics::Error error, vforensics::RunStats stats);

private:
  void run(JobRequest req);

  QString workdir_;
  uint64_t fixedClockMs_;
  mutable QMutex mutex_;
  std::atomic<bool> running_{false};
  std::atomic<bool> cancelFlag_{false};
  QString activeSequence_;
  QFuture<void> future_;
};

} // namespace vforensics
