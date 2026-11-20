#pragma once

#include <QObject>
#include <QRunnable>
#include <QString>

#include <functional>

class BackgroundJob : public QObject, public QRunnable {
    Q_OBJECT
public:
    explicit BackgroundJob(std::function<void()> work);
    void run() override;
    QString errorString() const { return error_; }

signals:
    void finished();

private:
    std::function<void()> work_;
    QString error_;
};
