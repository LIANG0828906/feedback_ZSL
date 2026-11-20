#pragma once

#include "core/Editing.h"
#include "core/Mesh.h"

#include <QOpenGLBuffer>
#include <QOpenGLFunctions_3_3_Core>
#include <QOpenGLVertexArrayObject>
#include <QOpenGLWidget>
#include <QMatrix4x4>
#include <QPoint>
#include <QSet>
#include <QVector3D>

#include <set>
#include <vector>

class QMouseEvent;
class QOpenGLShaderProgram;
class QPainter;
class QWheelEvent;

class MeshView : public QOpenGLWidget, protected QOpenGLFunctions_3_3_Core {
    Q_OBJECT
public:
    enum class Mode { Orbit, SelectFaces, SelectVertices };

    explicit MeshView(QWidget* parent = nullptr);
    ~MeshView() override;

    void setMesh(const meshedit::Mesh& mesh, bool resetCamera = true);
    void clearMesh();
    const meshedit::Mesh& mesh() const { return mesh_; }

    void setMode(Mode mode);
    Mode mode() const { return mode_; }
    meshedit::Selection selection() const;
    void clearSelection();

    void setHighlightFaces(const std::vector<int>& faces);
    void setValidationOverlay(const meshedit::Mesh& mesh,
                              const std::vector<int>& highlightFaces);
    void clearValidationOverlay();
    bool hasValidationOverlay() const { return overlayActive_; }

    void setWireframe(bool enabled);
    bool wireframe() const { return wireframe_; }
    void fitView();
    void releaseGlBuffers();

signals:
    void selectionChanged(int vertexCount, int faceCount);
    void validationOverlayChanged(bool active);

protected:
    void initializeGL() override;
    void resizeGL(int width, int height) override;
    void paintGL() override;
    void mousePressEvent(QMouseEvent* event) override;
    void mouseMoveEvent(QMouseEvent* event) override;
    void mouseReleaseEvent(QMouseEvent* event) override;
    void wheelEvent(QWheelEvent* event) override;
    void keyPressEvent(QKeyEvent* event) override;

private:
    struct Vertex {
        QVector3D position;
        QVector3D normal;
        QVector3D color;
    };

    QPoint eventPosition(QMouseEvent* event) const;
    void rebuildBuffers();
    void uploadVertices();
    void clearGpu();
    void applyBoxSelection(const QPoint& first, const QPoint& second);
    QPointF projectPoint(const meshedit::Vec3& point) const;
    bool faceVisible(int faceId) const;
    meshedit::Vec3 faceCenter(int faceId) const;
    QVector3D groupColor(const std::string& group) const;
    const meshedit::Mesh& displayMesh() const;
    QMatrix4x4 projectionMatrix() const;
    QMatrix4x4 viewMatrix() const;
    void drawSelectionMarkers(QPainter& painter);

    meshedit::Mesh mesh_;
    meshedit::Mesh overlay_;
    bool hasMesh_ = false;
    bool overlayActive_ = false;
    std::set<int> selectedFaces_;
    std::set<int> selectedVertices_;
    std::set<int> highlightFaces_;
    std::set<int> overlayHighlightFaces_;
    std::vector<Vertex> vertices_;
    QOpenGLShaderProgram* program_ = nullptr;
    QOpenGLBuffer vbo_{QOpenGLBuffer::VertexBuffer};
    QOpenGLVertexArrayObject vao_;
    bool glReady_ = false;
    bool buffersDirty_ = true;
    bool wireframe_ = false;
    Mode mode_ = Mode::Orbit;
    bool dragging_ = false;
    bool boxSelecting_ = false;
    QPoint lastPosition_;
    QPoint boxStart_;
    QPoint boxEnd_;
    float yaw_ = 35.0f;
    float pitch_ = 24.0f;
    float distance_ = 4.0f;
    QVector3D target_;
    QMatrix4x4 lastView_;
    QMatrix4x4 lastProjection_;
};
