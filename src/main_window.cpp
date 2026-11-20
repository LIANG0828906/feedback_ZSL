#include "main_window.h"
#include "timeline_widget.h"
#include "vforensics/frame_sequence.h"
#include "vforensics/report.h"
#include <QFileDialog>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QMessageBox>
#include <QPushButton>
#include <QVBoxLayout>
#include <opencv2/imgproc.hpp>

namespace vforensics {

MainWindow::MainWindow(QString workdir, QWidget* parent)
    : QMainWindow(parent), workdir_(std::move(workdir)), worker_(workdir_) {
  setWindowTitle(tr("Video Frame Forensics"));
  auto* central = new QWidget(this);
  auto* root = new QVBoxLayout(central);

  auto* bar = new QHBoxLayout();
  auto* refreshBtn = new QPushButton(tr("Reload published snapshot"), this);
  auto* exportBtn = new QPushButton(tr("Export JSON report"), this);
  bar->addWidget(refreshBtn);
  bar->addWidget(exportBtn);
  bar->addStretch();
  root->addLayout(bar);

  timeline_ = new TimelineWidget(this);
  root->addWidget(timeline_, 2);

  auto* bottom = new QHBoxLayout();
  detail_ = new QLabel(tr("Select a frame"), this);
  detail_->setAlignment(Qt::AlignTop | Qt::AlignLeft);
  detail_->setMinimumWidth(420);
  preview_ = new QLabel(this);
  preview_->setMinimumSize(200, 160);
  preview_->setFrameStyle(QFrame::Box);
  bottom->addWidget(detail_, 2);
  bottom->addWidget(preview_, 1);
  root->addLayout(bottom, 1);
  setCentralWidget(central);

  connect(refreshBtn, &QPushButton::clicked, this, &MainWindow::refreshPublished);
  connect(exportBtn, &QPushButton::clicked, this, &MainWindow::exportReport);
  connect(timeline_, &TimelineWidget::frameSelected, this, &MainWindow::onFrameSelected);
  connect(&worker_, &ForensicWorker::finished, this, [this](Error, RunStats) { refreshPublished(); });
  refreshPublished();
}

void MainWindow::setSequencePath(const QString& path) { sequencePath_ = path; }

void MainWindow::refreshPublished() {
  ForensicResult r;
  const Error e = worker_.readPublished(r);
  if (!e.ok()) {
    detail_->setText(tr("No readable published snapshot yet (%1)").arg(statusName(e.code)));
    return;
  }
  published_ = std::move(r);
  hasResult_ = true;
  timeline_->setResult(&published_);
}

void MainWindow::onFrameSelected(uint32_t frame) {
  if (!hasResult_ || frame >= published_.frames.size()) return;
  timeline_->setSelected(frame);
  const FrameVerification& fv = published_.frames[frame];
  QString path;
  for (size_t i = 0; i < fv.cascadePath.size(); ++i) {
    path += (i ? " -> " : "") + QString::number(fv.cascadePath[i]);
  }
  detail_->setText(QStringLiteral(
      "<b>Frame %1</b><br>Hash: %2<br>Verdict: %3<br>"
      "First mismatch byte (frame-local): %4<br>Absolute file offset: %5<br>"
      "Cascade state: %6<br>Dependency path: %7<br>Manifest version: v%8")
      .arg(frame)
      .arg(QString::fromLatin1(fv.contentHash.toHex()))
      .arg(fv.hashMatch ? tr("MATCH") : tr("MISMATCH"))
      .arg(fv.firstMismatchOffset)
      .arg(fv.firstMismatchAbsolute)
      .arg(fv.cascadeUntrusted ? tr("UNTRUSTED") : tr("TRUSTED"))
      .arg(path.isEmpty() ? tr("-") : path)
      .arg(published_.manifest.manifestVersion));

  // OpenCV pixel pipeline: load the raw frame from the sequence, convert to
  // grayscale and upscale so the preview is visible without a display.
  if (!sequencePath_.isEmpty()) {
    FrameSequence seq;
    if (FrameSequence::parse(sequencePath_, seq).ok() && frame < seq.header().frameCount) {
      const SequenceHeader& h = seq.header();
      const QByteArray raw = seq.framePayload(seq.frameHeaders()[frame]);
      const int type = h.channels == 1 ? CV_8UC1 : CV_8UC3;
      cv::Mat img(h.height, h.width, type,
                  const_cast<char*>(raw.constData()));
      cv::Mat gray;
      if (h.channels == 1) gray = img;
      else cv::cvtColor(img, gray, cv::COLOR_BGR2GRAY);
      cv::Mat scaled;
      cv::resize(gray, scaled, cv::Size(160, 120), 0, 0, cv::INTER_NEAREST);
      const QImage qimg(scaled.data, scaled.cols, scaled.rows,
                        static_cast<int>(scaled.step), QImage::Format_Grayscale8);
      preview_->setPixmap(QPixmap::fromImage(qimg.copy()));
    }
  }
}

void MainWindow::exportReport() {
  if (!hasResult_) {
    QMessageBox::warning(this, tr("Forensics"), tr("No published result to export."));
    return;
  }
  const QString path = QFileDialog::getSaveFileName(this, tr("Export report"),
                                                    workdir_ + "/diagnostic_report.json");
  if (path.isEmpty()) return;
  const Error e = DiagnosticReport::exportJson(published_, path);
  if (!e.ok()) {
    QMessageBox::critical(this, tr("Report failed"),
                          statusName(e.code) + ": " + e.message);
  }
}

} // namespace vforensics
