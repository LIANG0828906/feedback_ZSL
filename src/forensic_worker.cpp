#include "vforensics/forensic_worker.h"
#include <QtConcurrent/QtConcurrentRun>

namespace vforensics {

ForensicWorker::ForensicWorker(QString workdir, uint64_t fixedClockMs, QObject* parent)
    : QObject(parent), workdir_(std::move(workdir)), fixedClockMs_(fixedClockMs) {}

ForensicWorker::~ForensicWorker() {
  cancel();
  future_.waitForFinished();
}

bool ForensicWorker::isBusy() const {
  QMutexLocker lock(&mutex_);
  return running_.load();
}

Error ForensicWorker::submit(const JobRequest& req) {
  {
    QMutexLocker lock(&mutex_);
    if (running_.load()) {
      return Error::make(StatusCode::E_BUSY,
                         QStringLiteral("a verification task for %1 is already running")
                             .arg(activeSequence_));
    }
    running_ = true;
    cancelFlag_ = false;
    activeSequence_ = req.sequencePath;
  }
  future_ = QtConcurrent::run([this, req]() { run(req); });
  return Error::success();
}

Error ForensicWorker::cancel() {
  QMutexLocker lock(&mutex_);
  if (!running_.load()) return Error::success();
  cancelFlag_ = true;
  return Error::success();
}

Error ForensicWorker::readPublished(ForensicResult& out) const {
  SnapshotStore store(workdir_);
  QByteArray hash;
  uint64_t version = 0;
  return store.loadLatest(out, hash, version);
}

void ForensicWorker::run(JobRequest req) {
  ForensicEngine engine(workdir_, fixedClockMs_);
  RunStats stats;
  ForensicResult result;
  const auto progressCb = [this](uint32_t done, uint32_t total) {
    emit progress(done, total);
  };
  Error err = req.resume
      ? engine.resume(req.sequencePath, req.manifestPath, req.referencePath, stats, result)
      : engine.verify(req.sequencePath, req.manifestPath, req.referencePath, stats,
                      result, progressCb, &cancelFlag_);
  {
    QMutexLocker lock(&mutex_);
    running_ = false;
    activeSequence_.clear();
  }
  emit finished(err, stats);
}

} // namespace vforensics
