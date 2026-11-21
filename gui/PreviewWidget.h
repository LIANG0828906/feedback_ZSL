#pragma once

#include "font_atlas/types.hpp"

#include <QImage>
#include <QOpenGLWidget>
#include <QSet>
#include <QString>
#include <QVector>

class PreviewWidget : public QOpenGLWidget {
    Q_OBJECT

public:
    explicit PreviewWidget(QWidget* parent = nullptr);

    void setGlyphPreview(const QImage& image, const std::vector<font_atlas::ChunkPlan>& chunks);
    void setCompletedChunks(const QSet<int>& chunks);
    void clearPreview();
    QString openGlSummary() const { return openGlSummary_; }

protected:
    void initializeGL() override;
    void paintGL() override;

private:
    QImage image_;
    std::vector<font_atlas::ChunkPlan> chunks_;
    QSet<int> completed_;
    QString openGlSummary_ = QStringLiteral("OpenGL context not initialized");
};

