#pragma once
#include "vforensics/types.h"
#include <QWidget>

namespace vforensics {

// Paints the frame timeline: green trusted, red hash mismatch, orange
// cascaded-untrusted, with dependency edges highlighted when a frame is
// selected. It renders ONLY data from a published snapshot.
class TimelineWidget : public QWidget {
  Q_OBJECT
public:
  explicit TimelineWidget(QWidget* parent = nullptr);
  void setResult(const ForensicResult* result);
  void setSelected(uint32_t frame);

signals:
  void frameSelected(uint32_t frame);

protected:
  void paintEvent(QPaintEvent* event) override;
  void mousePressEvent(QMouseEvent* event) override;

private:
  const ForensicResult* result_ = nullptr;
  int selected_ = -1;
};

} // namespace vforensics
