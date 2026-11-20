#include "MeshView.h"

#include "core/Topology.h"

#include <QKeyEvent>
#include <QDebug>
#include <QMatrix4x4>
#include <QMouseEvent>
#include <QOpenGLShaderProgram>
#include <QPainter>
#include <QVector4D>
#include <QWheelEvent>

#include <algorithm>
#include <array>
#include <cstddef>
#include <cmath>
#include <cstring>

namespace {

QVector3D toQt(const meshedit::Vec3& value) {
    return QVector3D(static_cast<float>(value.x), static_cast<float>(value.y),
                     static_cast<float>(value.z));
}

const char* kVertexShader = R"(
#version 330 core
layout(location = 0) in vec3 aPosition;
layout(location = 1) in vec3 aNormal;
layout(location = 2) in vec3 aColor;
uniform mat4 uMvp;
out vec3 vNormal;
out vec3 vColor;
out vec3 vWorld;
void main() {
    vNormal = aNormal;
    vColor = aColor;
    vWorld = aPosition;
    gl_Position = uMvp * vec4(aPosition, 1.0);
}
)";

const char* kFragmentShader = R"(
#version 330 core
in vec3 vNormal;
in vec3 vColor;
in vec3 vWorld;
out vec4 fragColor;
void main() {
    vec3 n = normalize(vNormal);
    if (length(vNormal) < 1e-8) n = normalize(cross(dFdx(vWorld), dFdy(vWorld)));
    vec3 light = normalize(vec3(0.35, 0.55, 1.0));
    float diffuse = 0.30 + 0.70 * abs(dot(n, light));
    fragColor = vec4(vColor * diffuse, 1.0);
}
)";

} // namespace

MeshView::MeshView(QWidget* parent) : QOpenGLWidget(parent) {
    setFocusPolicy(Qt::StrongFocus);
    setMinimumSize(420, 320);
}

MeshView::~MeshView() {
    releaseGlBuffers();
}

void MeshView::setMesh(const meshedit::Mesh& mesh, bool resetCamera) {
    mesh_ = mesh;
    hasMesh_ = true;
    overlayActive_ = false;
    overlay_ = meshedit::Mesh{};
    selectedFaces_.clear();
    selectedVertices_.clear();
    highlightFaces_.clear();
    overlayHighlightFaces_.clear();
    buffersDirty_ = true;
    if (resetCamera) fitView();
    update();
    emit selectionChanged(0, 0);
    emit validationOverlayChanged(false);
}

void MeshView::clearMesh() {
    mesh_ = meshedit::Mesh{};
    overlay_ = meshedit::Mesh{};
    hasMesh_ = false;
    overlayActive_ = false;
    selectedFaces_.clear();
    selectedVertices_.clear();
    highlightFaces_.clear();
    overlayHighlightFaces_.clear();
    buffersDirty_ = true;
    update();
}

void MeshView::setMode(Mode mode) {
    mode_ = mode;
    setCursor(mode == Mode::Orbit ? Qt::OpenHandCursor : Qt::CrossCursor);
}

meshedit::Selection MeshView::selection() const {
    meshedit::Selection result;
    result.faces.assign(selectedFaces_.begin(), selectedFaces_.end());
    result.vertices.assign(selectedVertices_.begin(), selectedVertices_.end());
    return result;
}

void MeshView::clearSelection() {
    selectedFaces_.clear();
    selectedVertices_.clear();
    buffersDirty_ = true;
    update();
    emit selectionChanged(0, 0);
}

void MeshView::setHighlightFaces(const std::vector<int>& faces) {
    highlightFaces_.clear();
    for (int face : faces) {
        if (face >= 0 && static_cast<std::size_t>(face) < mesh_.faces.size()) {
            highlightFaces_.insert(face);
        }
    }
    buffersDirty_ = true;
    update();
}

void MeshView::setValidationOverlay(const meshedit::Mesh& mesh,
                                    const std::vector<int>& highlightFaces) {
    overlay_ = mesh;
    overlayActive_ = true;
    overlayHighlightFaces_.clear();
    for (int face : highlightFaces) {
        if (face >= 0 && static_cast<std::size_t>(face) < overlay_.faces.size()) {
            overlayHighlightFaces_.insert(face);
        }
    }
    buffersDirty_ = true;
    update();
    emit validationOverlayChanged(true);
}

void MeshView::clearValidationOverlay() {
    overlayActive_ = false;
    overlay_ = meshedit::Mesh{};
    overlayHighlightFaces_.clear();
    buffersDirty_ = true;
    update();
    emit validationOverlayChanged(false);
}

void MeshView::setWireframe(bool enabled) {
    wireframe_ = enabled;
    update();
}

void MeshView::fitView() {
    const meshedit::Mesh& active = displayMesh();
    const meshedit::MeshMetrics metrics = meshedit::computeMetrics(active);
    if (!metrics.bounds.valid()) {
        target_ = QVector3D();
        distance_ = 4.0f;
        return;
    }
    const meshedit::Vec3 center = metrics.bounds.center();
    target_ = toQt(center);
    const float radius = std::max(1e-5f, 0.5f * static_cast<float>(
        meshedit::length(metrics.bounds.extent())));
    distance_ = radius / std::tan(22.5f * 3.14159265358979323846f / 180.0f) * 1.25f;
    update();
}

void MeshView::releaseGlBuffers() {
    clearGpu();
    buffersDirty_ = true;
}

void MeshView::initializeGL() {
    initializeOpenGLFunctions();
    qInfo("OpenGL context: %s | renderer: %s",
          reinterpret_cast<const char*>(glGetString(GL_VERSION)),
          reinterpret_cast<const char*>(glGetString(GL_RENDERER)));
    glEnable(GL_DEPTH_TEST);
    glDisable(GL_CULL_FACE);
    glClearColor(0.075f, 0.09f, 0.11f, 1.0f);
    delete program_;
    program_ = new QOpenGLShaderProgram(this);
    if (!program_->addShaderFromSourceCode(QOpenGLShader::Vertex, kVertexShader) ||
        !program_->addShaderFromSourceCode(QOpenGLShader::Fragment, kFragmentShader) ||
        !program_->link()) {
        qWarning("MeshView shader setup failed: %s", qPrintable(program_->log()));
    }
    vao_.create();
    vbo_.create();
    glReady_ = true;
    buffersDirty_ = true;
}

void MeshView::resizeGL(int width, int height) {
    glViewport(0, 0, width, height);
}

QMatrix4x4 MeshView::projectionMatrix() const {
    QMatrix4x4 projection;
    const float aspect = height() > 0 ? static_cast<float>(width()) / height() : 1.0f;
    const float radius = std::max(1e-4f, distance_);
    projection.perspective(45.0f, aspect, radius * 0.001f, radius * 100.0f + 1000.0f);
    return projection;
}

QMatrix4x4 MeshView::viewMatrix() const {
    QMatrix4x4 view;
    view.translate(0.0f, 0.0f, -distance_);
    view.rotate(pitch_, 1.0f, 0.0f, 0.0f);
    view.rotate(yaw_, 0.0f, 1.0f, 0.0f);
    view.translate(-target_);
    return view;
}

const meshedit::Mesh& MeshView::displayMesh() const {
    return overlayActive_ ? overlay_ : mesh_;
}

QVector3D MeshView::groupColor(const std::string& group) const {
    const meshedit::Mesh& active = displayMesh();
    auto found = std::find(active.groups.begin(), active.groups.end(), group);
    const std::size_t index = found == active.groups.end()
        ? 0 : static_cast<std::size_t>(found - active.groups.begin());
    static const QVector3D palette[] = {
        QVector3D(0.36f, 0.66f, 0.92f),
        QVector3D(0.36f, 0.82f, 0.61f),
        QVector3D(0.92f, 0.66f, 0.32f),
        QVector3D(0.76f, 0.50f, 0.86f),
        QVector3D(0.32f, 0.82f, 0.82f),
        QVector3D(0.88f, 0.48f, 0.56f)};
    return palette[index % (sizeof(palette) / sizeof(palette[0]))];
}

void MeshView::rebuildBuffers() {
    const meshedit::Mesh& active = displayMesh();
    vertices_.clear();
    if (!active.faces.empty()) {
        vertices_.reserve(active.faces.size() * 3);
    }
    const std::set<int>& activeHighlights =
        overlayActive_ ? overlayHighlightFaces_ : highlightFaces_;
    for (std::size_t fi = 0; fi < active.faces.size(); ++fi) {
        const meshedit::Face& face = active.faces[fi];
        const int ia = face.corners[0].position;
        const int ib = face.corners[1].position;
        const int ic = face.corners[2].position;
        if (ia < 0 || ib < 0 || ic < 0 ||
            static_cast<std::size_t>(ia) >= active.positions.size() ||
            static_cast<std::size_t>(ib) >= active.positions.size() ||
            static_cast<std::size_t>(ic) >= active.positions.size()) {
            continue;
        }
        QVector3D a = toQt(active.positions[static_cast<std::size_t>(ia)]);
        QVector3D b = toQt(active.positions[static_cast<std::size_t>(ib)]);
        QVector3D c = toQt(active.positions[static_cast<std::size_t>(ic)]);
        QVector3D geometricNormal = QVector3D::crossProduct(b - a, c - a);
        if (!geometricNormal.isNull()) geometricNormal.normalize();
        QVector3D color = groupColor(face.group);
        if (activeHighlights.count(static_cast<int>(fi))) color = QVector3D(0.96f, 0.18f, 0.16f);
        else if (!overlayActive_ && selectedFaces_.count(static_cast<int>(fi))) {
            color = QVector3D(1.0f, 0.82f, 0.18f);
        }
        const QVector3D positions[3] = {a, b, c};
        for (int i = 0; i < 3; ++i) {
            const meshedit::Corner& corner = face.corners[static_cast<std::size_t>(i)];
            QVector3D normal = geometricNormal;
            if (corner.normal >= 0 &&
                static_cast<std::size_t>(corner.normal) < active.normals.size()) {
                normal = toQt(active.normals[static_cast<std::size_t>(corner.normal)]);
            }
            vertices_.push_back({positions[i], normal, color});
        }
    }
    uploadVertices();
}

void MeshView::uploadVertices() {
    if (!glReady_) return;
    vao_.bind();
    vbo_.bind();
    const int bytes = static_cast<int>(vertices_.size() * sizeof(Vertex));
    if (bytes == 0) {
        vbo_.allocate(nullptr, 0);
    } else {
        vbo_.allocate(vertices_.data(), bytes);
        glEnableVertexAttribArray(0);
        glVertexAttribPointer(0, 3, GL_FLOAT, GL_FALSE, sizeof(Vertex),
                              reinterpret_cast<void*>(offsetof(Vertex, position)));
        glEnableVertexAttribArray(1);
        glVertexAttribPointer(1, 3, GL_FLOAT, GL_FALSE, sizeof(Vertex),
                              reinterpret_cast<void*>(offsetof(Vertex, normal)));
        glEnableVertexAttribArray(2);
        glVertexAttribPointer(2, 3, GL_FLOAT, GL_FALSE, sizeof(Vertex),
                              reinterpret_cast<void*>(offsetof(Vertex, color)));
    }
    vbo_.release();
    vao_.release();
    buffersDirty_ = false;
}

void MeshView::clearGpu() {
    if (!glReady_) return;
    if (!context()) {
        glReady_ = false;
        return;
    }
    makeCurrent();
    if (vbo_.isCreated()) vbo_.destroy();
    if (vao_.isCreated()) vao_.destroy();
    doneCurrent();
    glReady_ = false;
}

void MeshView::paintGL() {
    glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
    glEnable(GL_DEPTH_TEST);
    if (!glReady_ && program_) {
        vao_.create();
        vbo_.create();
        glReady_ = true;
        buffersDirty_ = true;
    }
    if (buffersDirty_) rebuildBuffers();
    lastProjection_ = projectionMatrix();
    lastView_ = viewMatrix();
    if (program_ && !vertices_.empty()) {
        program_->bind();
        QMatrix4x4 mvp = lastProjection_ * lastView_;
        program_->setUniformValue("uMvp", mvp);
        glPolygonMode(GL_FRONT_AND_BACK, wireframe_ ? GL_LINE : GL_FILL);
        vao_.bind();
        glDrawArrays(GL_TRIANGLES, 0, static_cast<GLsizei>(vertices_.size()));
        vao_.release();
        glPolygonMode(GL_FRONT_AND_BACK, GL_FILL);
        program_->release();
    }

    QPainter painter(this);
    painter.setRenderHint(QPainter::Antialiasing);
    drawSelectionMarkers(painter);
    if (boxSelecting_ && (boxEnd_ - boxStart_).manhattanLength() > 3) {
        painter.setPen(QPen(QColor(255, 190, 50), 1, Qt::DashLine));
        painter.setBrush(QColor(255, 190, 50, 35));
        painter.drawRect(QRect(boxStart_, boxEnd_).normalized());
    }
    if (overlayActive_) {
        painter.setPen(QColor(255, 210, 90));
        painter.drawText(12, 22, tr("Validation candidate overlay - Esc to clear"));
    }
}

QPointF MeshView::projectPoint(const meshedit::Vec3& point) const {
    const QVector4D clip = lastProjection_ * lastView_ *
        QVector4D(static_cast<float>(point.x), static_cast<float>(point.y),
                  static_cast<float>(point.z), 1.0f);
    if (clip.w() <= 1e-8f) return QPointF(-1e9, -1e9);
    const QVector3D ndc = clip.toVector3D() / clip.w();
    return QPointF((ndc.x() * 0.5f + 0.5f) * width(),
                   (1.0f - (ndc.y() * 0.5f + 0.5f)) * height());
}

meshedit::Vec3 MeshView::faceCenter(int faceId) const {
    const meshedit::Mesh& active = displayMesh();
    meshedit::Vec3 center;
    int count = 0;
    if (faceId < 0 || static_cast<std::size_t>(faceId) >= active.faces.size()) return center;
    for (const meshedit::Corner& corner : active.faces[static_cast<std::size_t>(faceId)].corners) {
        if (corner.position >= 0 &&
            static_cast<std::size_t>(corner.position) < active.positions.size()) {
            center += active.positions[static_cast<std::size_t>(corner.position)];
            ++count;
        }
    }
    return count > 0 ? center / count : center;
}

bool MeshView::faceVisible(int faceId) const {
    return faceId >= 0 && static_cast<std::size_t>(faceId) < displayMesh().faces.size();
}

void MeshView::applyBoxSelection(const QPoint& first, const QPoint& second) {
    if (overlayActive_) return;
    const QRect box = QRect(first, second).normalized();
    selectedFaces_.clear();
    selectedVertices_.clear();
    if (mode_ == Mode::SelectVertices) {
        for (std::size_t i = 0; i < mesh_.positions.size(); ++i) {
            if (box.contains(projectPoint(mesh_.positions[i]).toPoint())) {
                selectedVertices_.insert(static_cast<int>(i));
            }
        }
    } else if (mode_ == Mode::SelectFaces) {
        for (std::size_t fi = 0; fi < mesh_.faces.size(); ++fi) {
            if (box.contains(projectPoint(faceCenter(static_cast<int>(fi))).toPoint())) {
                selectedFaces_.insert(static_cast<int>(fi));
                for (const meshedit::Corner& corner : mesh_.faces[fi].corners) {
                    selectedVertices_.insert(corner.position);
                }
            }
        }
    }
    buffersDirty_ = true;
    update();
    emit selectionChanged(static_cast<int>(selectedVertices_.size()),
                          static_cast<int>(selectedFaces_.size()));
}

void MeshView::drawSelectionMarkers(QPainter& painter) {
    if (overlayActive_) return;
    painter.setPen(QPen(QColor(255, 220, 40), 1.5));
    painter.setBrush(QColor(255, 220, 40, 180));
    for (int vertex : selectedVertices_) {
        if (vertex < 0 || static_cast<std::size_t>(vertex) >= mesh_.positions.size()) continue;
        const QPointF point = projectPoint(mesh_.positions[static_cast<std::size_t>(vertex)]);
        if (point.x() < -1e8) continue;
        painter.drawEllipse(point, 2.7, 2.7);
    }
}

QPoint MeshView::eventPosition(QMouseEvent* event) const {
#if MESHEDITOR_QT_MAJOR >= 6
    return event->position().toPoint();
#else
    return event->pos();
#endif
}

void MeshView::mousePressEvent(QMouseEvent* event) {
    setFocus();
    lastPosition_ = eventPosition(event);
    dragging_ = true;
    if (event->button() == Qt::LeftButton &&
        (mode_ == Mode::SelectFaces || mode_ == Mode::SelectVertices) &&
        !overlayActive_) {
        boxSelecting_ = true;
        boxStart_ = lastPosition_;
        boxEnd_ = lastPosition_;
    }
    event->accept();
}

void MeshView::mouseMoveEvent(QMouseEvent* event) {
    if (!dragging_) {
        event->accept();
        return;
    }
    const QPoint position = eventPosition(event);
    const QPoint delta = position - lastPosition_;
    if (boxSelecting_) {
        boxEnd_ = position;
        update();
    } else if (event->buttons() & Qt::LeftButton && mode_ == Mode::Orbit) {
        yaw_ += delta.x() * 0.45f;
        pitch_ = std::clamp(pitch_ + delta.y() * 0.35f, -89.0f, 89.0f);
        update();
    } else if (event->buttons() & (Qt::MiddleButton | Qt::RightButton)) {
        const float scale = distance_ * 0.0016f;
        const QMatrix4x4 inverse = (lastProjection_ * lastView_).inverted();
        const QVector3D right = inverse.mapVector(QVector3D(1, 0, 0));
        const QVector3D up = inverse.mapVector(QVector3D(0, 1, 0));
        target_ += (-right * static_cast<float>(delta.x()) +
                    up * static_cast<float>(delta.y())) * scale;
        update();
    }
    lastPosition_ = position;
    event->accept();
}

void MeshView::mouseReleaseEvent(QMouseEvent* event) {
    if (boxSelecting_) {
        boxEnd_ = eventPosition(event);
        boxSelecting_ = false;
        applyBoxSelection(boxStart_, boxEnd_);
    }
    dragging_ = false;
    update();
    event->accept();
}

void MeshView::wheelEvent(QWheelEvent* event) {
    const float steps = event->angleDelta().y() / 120.0f;
    distance_ = std::clamp(distance_ * std::pow(0.85f, steps), 1e-5f, 1e12f);
    update();
    event->accept();
}

void MeshView::keyPressEvent(QKeyEvent* event) {
    if (event->key() == Qt::Key_Escape) {
        if (overlayActive_) clearValidationOverlay();
        else clearSelection();
        return;
    }
    if (event->key() == Qt::Key_F) {
        fitView();
        return;
    }
    QOpenGLWidget::keyPressEvent(event);
}
