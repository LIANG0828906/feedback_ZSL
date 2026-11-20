#include "BackgroundJob.h"

#include <exception>

BackgroundJob::BackgroundJob(std::function<void()> work)
    : work_(std::move(work)) {
    setAutoDelete(false);
}

void BackgroundJob::run() {
    try {
        if (work_) work_();
    } catch (const std::exception& exception) {
        error_ = QString::fromUtf8(exception.what());
    } catch (...) {
        error_ = QStringLiteral("unknown background exception");
    }
    emit finished();
}
