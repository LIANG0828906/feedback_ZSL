#include "PreviewWidget.h"

#include <QOpenGLContext>
#include <QPainter>
#include <QSurfaceFormat>

#include <algorithm>

PreviewWidget::PreviewWidget(QWidget* parent) : QOpenGLWidget(parent) {
    setMinimumSize(420, 420);
    QSurfaceFormat format;
    format.setVersion(3, 3);
    format.setProfile(QSurfaceFormat::CoreProfile);
    format.setDepthBufferSize(24);
    setFormat(format);
}

void PreviewWidget::setGlyphPreview(
    const QImage& image, const std::vector<font_atlas::ChunkPlan>& chunks) {
    image_ = image;
    chunks_ = chunks;
    completed_.clear();
    update();
}

void PreviewWidget::setCompletedChunks(const QSet<int>& chunks) {
    completed_ = chunks;
    update();
}

void PreviewWidget::clearPreview() {
    image_ = QImage();
    chunks_.clear();
    completed_.clear();
    update();
}

void PreviewWidget::initializeGL() {
    if (context() != nullptr) {
        const QSurfaceFormat format = context()->format();
        openGlSummary_ =
            QStringLiteral("OpenGL %1.%2 %3")
                .arg(format.majorVersion())
                .arg(format.minorVersion())
                .arg(format.profile() == QSurfaceFormat::CoreProfile
                         ? QStringLiteral("Core")
                         : QStringLiteral("Compatibility"));
    }
}

void PreviewWidget::paintGL() {
    QPainter painter(this);
    painter.fillRect(rect(), QColor(28, 30, 34));

    const int cell = 16;
    for (int y = 0; y < height(); y += cell) {
        for (int x = 0; x < width(); x += cell) {
            const QColor color = ((x / cell + y / cell) & 1) ? QColor(45, 48, 54)
                                                             : QColor(38, 41, 47);
            painter.fillRect(x, y, cell, cell, color);
        }
    }

    if (image_.isNull()) {
        painter.setPen(QColor(180, 185, 195));
        painter.drawText(rect(), Qt::AlignCenter,
                         QStringLiteral("Load a font and start rasterization"));
        return;
    }

    const double scale = std::min(
        1.0, std::min((width() - 40.0) / image_.width(),
                      (height() - 40.0) / image_.height()));
    const int drawWidth = std::max(1, static_cast<int>(image_.width() * scale));
    const int drawHeight = std::max(1, static_cast<int>(image_.height() * scale));
    const int originX = (width() - drawWidth) / 2;
    const int originY = (height() - drawHeight) / 2;
    const QRect target(originX, originY, drawWidth, drawHeight);
    painter.setRenderHint(QPainter::SmoothPixmapTransform, false);
    painter.drawImage(target, image_);

    painter.setBrush(Qt::NoBrush);
    for (const auto& chunk : chunks_) {
        const QRect chunkRect(
            originX + static_cast<int>(chunk.rect.x * scale),
            originY + static_cast<int>(chunk.rect.y * scale),
            std::max(1, static_cast<int>(chunk.rect.width * scale)),
            std::max(1, static_cast<int>(chunk.rect.height * scale)));
        if (completed_.contains(chunk.index)) {
            painter.fillRect(chunkRect, QColor(58, 200, 120, 55));
        }
        painter.setPen(QPen(QColor(90, 170, 235, 180), 1));
        painter.drawRect(chunkRect);
    }

    painter.setPen(QColor(220, 225, 235));
    painter.drawText(QRect(12, height() - 34, width() - 24, 24),
                     Qt::AlignLeft | Qt::AlignVCenter, openGlSummary_);
}

