#include "timeline_widget.h"
#include <QMouseEvent>
#include <QPainter>
#include <QToolTip>

namespace vforensics {

TimelineWidget::TimelineWidget(QWidget* parent) : QWidget(parent) {
  setMinimumHeight(140);
  setMouseTracking(true);
}

void TimelineWidget::setResult(const ForensicResult* result) {
  result_ = result;
  selected_ = -1;
  update();
}

void TimelineWidget::setSelected(uint32_t frame) {
  selected_ = static_cast<int>(frame);
  update();
}

void TimelineWidget::paintEvent(QPaintEvent*) {
  QPainter p(this);
  p.fillRect(rect(), Qt::white);
  if (!result_ || result_->frames.empty()) {
    p.drawText(rect(), Qt::AlignCenter, tr("No published forensic result"));
    return;
  }
  const int n = static_cast<int>(result_->frames.size());
  const int margin = 20;
  const int slot = std::max(28, (width() - 2 * margin) / n);
  const int boxW = std::min(24, slot - 6);
  const int y = 50;

  QFont small = p.font();
  small.setPointSize(8);
  p.setFont(small);

  for (int i = 0; i < n; ++i) {
    const FrameVerification& fv = result_->frames[i];
    QColor color = Qt::green;
    if (!fv.hashMatch) color = Qt::red;
    else if (fv.cascadeUntrusted) color = QColor(255, 140, 0);
    const int x = margin + i * slot;
    p.setBrush(color);
    p.setPen(i == selected_ ? QPen(Qt::blue, 3) : QPen(Qt::darkGray, 1));
    p.drawRect(x, y, boxW, 36);
    p.setPen(Qt::black);
    p.drawText(QRect(x, y + 40, boxW, 16), Qt::AlignCenter, QString::number(i));
    if (!fv.hashMatch) {
      p.drawText(QRect(x, y - 18, boxW + 20, 16), Qt::AlignCenter,
                 QStringLiteral("off:%1").arg(fv.firstMismatchOffset));
    }
  }

  // Dependency edges: child references its parent (i-1 style chains).
  p.setPen(QPen(QColor(120, 120, 120), 1, Qt::DotLine));
  for (int i = 1; i < n; ++i) {
    const int x1 = margin + (i - 1) * slot + boxW / 2;
    const int x2 = margin + i * slot + boxW / 2;
    p.drawLine(x1, y + 18, x2, y + 18);
  }

  if (selected_ >= 0 && selected_ < n) {
    const FrameVerification& fv = result_->frames[selected_];
    p.setPen(QPen(Qt::blue, 2));
    for (uint32_t node : fv.cascadePath) {
      const int x = margin + static_cast<int>(node) * slot;
      p.drawRect(x - 2, y - 2, boxW + 4, 40);
    }
    p.setPen(Qt::black);
    p.drawText(QRect(margin, 4, width() - 2 * margin, 20), Qt::AlignLeft,
               tr("Frame %1  %2  cascade path: %3")
                   .arg(selected_)
                   .arg(fv.hashMatch ? tr("hash MATCH") : tr("hash MISMATCH"))
                   .arg([&] {
                      QString s;
                      for (size_t k = 0; k < fv.cascadePath.size(); ++k)
                        s += (k ? "->" : "") + QString::number(fv.cascadePath[k]);
                      return s;
                    }()));
  }
}

void TimelineWidget::mousePressEvent(QMouseEvent* event) {
  if (!result_) return;
  const int n = static_cast<int>(result_->frames.size());
  const int margin = 20;
  const int slot = std::max(28, (width() - 2 * margin) / n);
  const int idx = (event->pos().x() - margin) / slot;
  if (idx >= 0 && idx < n && event->pos().y() >= 40 && event->pos().y() <= 100) {
    selected_ = idx;
    update();
    emit frameSelected(static_cast<uint32_t>(idx));
  }
}

} // namespace vforensics
