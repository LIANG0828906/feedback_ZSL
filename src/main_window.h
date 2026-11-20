#pragma once
#include "vforensics/forensic_worker.h"
#include <QMainWindow>
#include <QPixmap>

class QLabel;
class QListWidget;
class QListWidgetItem;

namespace vforensics {

class TimelineWidget;

class MainWindow : public QMainWindow {
  Q_OBJECT
public:
  explicit MainWindow(QString workdir, QWidget* parent = nullptr);
  void setSequencePath(const QString& path);

private slots:
  void refreshPublished();
  void onFrameSelected(uint32_t frame);
  void exportReport();

private:
  QString workdir_;
  QString sequencePath_;
  ForensicWorker worker_;
  ForensicResult published_;
  bool hasResult_ = false;
  TimelineWidget* timeline_ = nullptr;
  QLabel* detail_ = nullptr;
  QLabel* preview_ = nullptr;
};

} // namespace vforensics
