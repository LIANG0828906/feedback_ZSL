#include "MainWindow.h"

#include "BackgroundJob.h"
#include "Dialogs.h"
#include "MeshView.h"

#include "core/Format.h"
#include "core/VersionCompat.h"

#include <QAction>
#include <QActionGroup>
#include <QApplication>
#include <QComboBox>
#include <QDir>
#include <QDockWidget>
#include <QFileDialog>
#include <QFileInfo>
#include <QFormLayout>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QMenu>
#include <QMenuBar>
#include <QMessageBox>
#include <QPlainTextEdit>
#include <QPointer>
#include <QPushButton>
#include <QScrollBar>
#include <QStatusBar>
#include <QToolBar>
#include <QVBoxLayout>

#include <cmath>
#include <exception>

namespace {

QString fromUtf8(const std::string& text) {
    return QString::fromUtf8(text.data(), static_cast<int>(text.size()));
}

struct ValidationJobResult {
    meshedit::Error error;
    meshedit::ConversionReport conversion;
    meshedit::ValidationReport report;
    meshedit::Mesh candidate;
    bool haveReport = false;
};

} // namespace

MainWindow::MainWindow(QWidget* parent) : QMainWindow(parent) {
    setWindowTitle(tr("MeshEditor"));
    resize(1360, 860);
    pool_.setMaxThreadCount(2);

    view_ = new MeshView(this);
    setCentralWidget(view_);
    createActions();
    createMenus();
    createDocks();

    statusBar()->showMessage(tr("Open an OBJ or STL mesh to begin"));
    connect(view_, &MeshView::selectionChanged, this,
            [this](int vertices, int faces) {
                if (!selectionLabel_) return;
                selectionLabel_->setText(
                    tr("Selected: %1 vertices, %2 faces").arg(vertices).arg(faces));
            });
    connect(view_, &MeshView::validationOverlayChanged, this,
            [this](bool active) {
                overlayLabel_->setText(active
                    ? tr("Validation overlay active (candidate mesh)")
                    : tr("No validation overlay"));
                clearOverlayAction_->setEnabled(active);
            });
    updateActions();
    updateMetrics();
}

MainWindow::~MainWindow() {
    if (taskToken_) taskToken_->cancel();
    taskLease_.release();
    pool_.waitForDone();
}

void MainWindow::createActions() {
    openAction_ = new QAction(tr("&Open mesh..."), this);
    openAction_->setShortcut(QKeySequence::Open);
    connect(openAction_, &QAction::triggered, this, &MainWindow::openMesh);

    openProjectAction_ = new QAction(tr("Open &project..."), this);
    connect(openProjectAction_, &QAction::triggered, this, &MainWindow::openProject);

    saveProjectAction_ = new QAction(tr("&Save project..."), this);
    saveProjectAction_->setShortcut(QKeySequence::Save);
    connect(saveProjectAction_, &QAction::triggered, this, &MainWindow::saveProject);

    exportAction_ = new QAction(tr("&Export mesh..."), this);
    connect(exportAction_, &QAction::triggered, this, &MainWindow::exportMesh);

    validateAction_ = new QAction(tr("Export and &validate..."), this);
    connect(validateAction_, &QAction::triggered, this, &MainWindow::validateExport);

    undoAction_ = new QAction(tr("&Undo"), this);
    undoAction_->setShortcut(QKeySequence::Undo);
    connect(undoAction_, &QAction::triggered, this, &MainWindow::undo);
    redoAction_ = new QAction(tr("&Redo"), this);
    redoAction_->setShortcut(QKeySequence::Redo);
    connect(redoAction_, &QAction::triggered, this, &MainWindow::redo);

    weldAction_ = new QAction(tr("&Weld vertices..."), this);
    connect(weldAction_, &QAction::triggered, this, &MainWindow::weldMesh);
    simplifyAction_ = new QAction(tr("&Simplify..."), this);
    connect(simplifyAction_, &QAction::triggered, this, &MainWindow::simplifyMesh);
    transformAction_ = new QAction(tr("&Transform selection..."), this);
    connect(transformAction_, &QAction::triggered, this, &MainWindow::transformSelection);

    cancelAction_ = new QAction(tr("Cancel background task"), this);
    cancelAction_->setEnabled(false);
    connect(cancelAction_, &QAction::triggered, this, &MainWindow::cancelTask);

    clearOverlayAction_ = new QAction(tr("Clear validation overlay"), this);
    clearOverlayAction_->setEnabled(false);
    connect(clearOverlayAction_, &QAction::triggered, this, &MainWindow::clearOverlay);
}

void MainWindow::createMenus() {
    auto* fileMenu = menuBar()->addMenu(tr("&File"));
    fileMenu->addAction(openAction_);
    fileMenu->addAction(openProjectAction_);
    fileMenu->addSeparator();
    fileMenu->addAction(saveProjectAction_);
    fileMenu->addAction(exportAction_);
    fileMenu->addAction(validateAction_);
    fileMenu->addSeparator();
    fileMenu->addAction(tr("E&xit"), this, &QWidget::close);

    auto* editMenu = menuBar()->addMenu(tr("&Edit"));
    editMenu->addAction(undoAction_);
    editMenu->addAction(redoAction_);
    editMenu->addAction(transformAction_);
    editMenu->addAction(cancelAction_);

    auto* meshMenu = menuBar()->addMenu(tr("&Mesh"));
    meshMenu->addAction(weldAction_);
    meshMenu->addAction(simplifyAction_);

    auto* viewMenu = menuBar()->addMenu(tr("&View"));
    auto* fitAction = new QAction(tr("&Fit mesh"), this);
    fitAction->setShortcut(Qt::Key_F);
    connect(fitAction, &QAction::triggered, this, &MainWindow::fitView);
    viewMenu->addAction(fitAction);
    auto* wireframeAction = new QAction(tr("&Wireframe"), this);
    wireframeAction->setCheckable(true);
    connect(wireframeAction, &QAction::toggled, this,
            [this](bool checked) { view_->setWireframe(checked); });
    viewMenu->addAction(wireframeAction);
    viewMenu->addAction(clearOverlayAction_);
    viewMenu->addSeparator();

    auto* modeGroup = new QActionGroup(this);
    modeGroup->setExclusive(true);
    auto* orbit = new QAction(tr("Orbit"), this);
    orbit->setCheckable(true);
    orbit->setChecked(true);
    auto* faces = new QAction(tr("Box select faces"), this);
    faces->setCheckable(true);
    auto* vertices = new QAction(tr("Box select vertices"), this);
    vertices->setCheckable(true);
    modeGroup->addAction(orbit);
    modeGroup->addAction(faces);
    modeGroup->addAction(vertices);
    viewMenu->addActions(modeGroup->actions());
    connect(orbit, &QAction::triggered, this,
            [this] { view_->setMode(MeshView::Mode::Orbit); });
    connect(faces, &QAction::triggered, this,
            [this] { view_->setMode(MeshView::Mode::SelectFaces); });
    connect(vertices, &QAction::triggered, this,
            [this] { view_->setMode(MeshView::Mode::SelectVertices); });

    auto* toolbar = addToolBar(tr("Main"));
    toolbar->setMovable(false);
    toolbar->addAction(openAction_);
    toolbar->addAction(saveProjectAction_);
    toolbar->addAction(exportAction_);
    toolbar->addAction(validateAction_);
    toolbar->addSeparator();
    toolbar->addAction(undoAction_);
    toolbar->addAction(redoAction_);
    toolbar->addSeparator();
    toolbar->addAction(weldAction_);
    toolbar->addAction(simplifyAction_);
    toolbar->addAction(transformAction_);
    toolbar->addSeparator();
    toolbar->addAction(cancelAction_);
}

void MainWindow::createDocks() {
    auto* left = new QDockWidget(tr("Selection and metrics"), this);
    left->setAllowedAreas(Qt::LeftDockWidgetArea | Qt::RightDockWidgetArea);
    auto* leftWidget = new QWidget(left);
    auto* leftLayout = new QVBoxLayout(leftWidget);
    selectionLabel_ = new QLabel(tr("Selected: 0 vertices, 0 faces"), leftWidget);
    leftLayout->addWidget(selectionLabel_);

    auto* modeBox = new QGroupBox(tr("Viewport mode"), leftWidget);
    auto* modeLayout = new QVBoxLayout(modeBox);
    auto* orbitButton = new QPushButton(tr("Orbit / rotate"), modeBox);
    auto* faceButton = new QPushButton(tr("Box select faces"), modeBox);
    auto* vertexButton = new QPushButton(tr("Box select vertices"), modeBox);
    modeLayout->addWidget(orbitButton);
    modeLayout->addWidget(faceButton);
    modeLayout->addWidget(vertexButton);
    connect(orbitButton, &QPushButton::clicked, this,
            [this] { view_->setMode(MeshView::Mode::Orbit); });
    connect(faceButton, &QPushButton::clicked, this,
            [this] { view_->setMode(MeshView::Mode::SelectFaces); });
    connect(vertexButton, &QPushButton::clicked, this,
            [this] { view_->setMode(MeshView::Mode::SelectVertices); });
    leftLayout->addWidget(modeBox);

    auto* editBox = new QGroupBox(tr("Operations"), leftWidget);
    auto* editLayout = new QVBoxLayout(editBox);
    auto* transformButton = new QPushButton(tr("Transform selection..."), editBox);
    auto* weldButton = new QPushButton(tr("Weld vertices..."), editBox);
    auto* simplifyButton = new QPushButton(tr("Simplify mesh..."), editBox);
    auto* validateButton = new QPushButton(tr("Export + validate..."), editBox);
    editLayout->addWidget(transformButton);
    editLayout->addWidget(weldButton);
    editLayout->addWidget(simplifyButton);
    editLayout->addWidget(validateButton);
    connect(transformButton, &QPushButton::clicked, this, &MainWindow::transformSelection);
    connect(weldButton, &QPushButton::clicked, this, &MainWindow::weldMesh);
    connect(simplifyButton, &QPushButton::clicked, this, &MainWindow::simplifyMesh);
    connect(validateButton, &QPushButton::clicked, this, &MainWindow::validateExport);
    leftLayout->addWidget(editBox);

    metricsLabel_ = new QLabel(leftWidget);
    metricsLabel_->setWordWrap(true);
    metricsLabel_->setTextInteractionFlags(Qt::TextSelectableByMouse);
    leftLayout->addWidget(new QLabel(tr("Geometry metrics"), leftWidget));
    leftLayout->addWidget(metricsLabel_);
    overlayLabel_ = new QLabel(tr("No validation overlay"), leftWidget);
    overlayLabel_->setWordWrap(true);
    leftLayout->addWidget(overlayLabel_);
    leftLayout->addStretch();
    left->setWidget(leftWidget);
    addDockWidget(Qt::LeftDockWidgetArea, left);

    auto* right = new QDockWidget(tr("Trace, losses and validation"), this);
    traceView_ = new QPlainTextEdit(right);
    traceView_->setReadOnly(true);
    traceView_->setLineWrapMode(QPlainTextEdit::NoWrap);
    right->setWidget(traceView_);
    addDockWidget(Qt::RightDockWidgetArea, right);
    resizeDocks({left, right}, {300, 430}, Qt::Horizontal);
}

void MainWindow::updateActions() {
    const bool hasMesh = !document_.current.faces.empty();
    const bool idle = !taskRunning_;
    openAction_->setEnabled(idle);
    openProjectAction_->setEnabled(idle);
    saveProjectAction_->setEnabled(hasMesh && idle);
    exportAction_->setEnabled(hasMesh && idle);
    validateAction_->setEnabled(hasMesh && idle);
    weldAction_->setEnabled(hasMesh && idle);
    simplifyAction_->setEnabled(hasMesh && idle);
    transformAction_->setEnabled(hasMesh && idle);
    undoAction_->setEnabled(hasMesh && idle && document_.history.canUndo());
    redoAction_->setEnabled(hasMesh && idle && document_.history.canRedo());
    cancelAction_->setEnabled(taskRunning_);
    clearOverlayAction_->setEnabled(view_->hasValidationOverlay());
}

void MainWindow::updateMetrics() {
    if (!metricsLabel_) return;
    if (document_.current.faces.empty()) {
        metricsLabel_->setText(tr("No mesh loaded."));
        return;
    }
    const auto metrics = document_.metrics();
    QString bounds = tr("invalid");
    if (metrics.bounds.valid()) {
        bounds = tr("[%1, %2, %3] .. [%4, %5, %6]")
            .arg(metrics.bounds.min.x, 0, 'g', 8)
            .arg(metrics.bounds.min.y, 0, 'g', 8)
            .arg(metrics.bounds.min.z, 0, 'g', 8)
            .arg(metrics.bounds.max.x, 0, 'g', 8)
            .arg(metrics.bounds.max.y, 0, 'g', 8)
            .arg(metrics.bounds.max.z, 0, 'g', 8);
    }
    metricsLabel_->setText(
        tr("Vertices: %1\nFaces: %2\nBounds: %3\nVolume estimate: %4\nSurface area: %5\n"
           "Undo: %6  Redo: %7")
            .arg(metrics.vertexCount)
            .arg(metrics.faceCount)
            .arg(bounds)
            .arg(metrics.volume, 0, 'g', 10)
            .arg(metrics.surfaceArea, 0, 'g', 10)
            .arg(document_.history.undoCount())
            .arg(document_.history.redoCount()));
}

void MainWindow::appendTrace(const QString& text) {
    if (!traceView_) return;
    traceView_->appendPlainText(text);
    traceView_->verticalScrollBar()->setValue(traceView_->verticalScrollBar()->maximum());
}

void MainWindow::displayError(const meshedit::Error& error) {
    QString text = QStringLiteral("%1: %2")
        .arg(QString::fromLatin1(meshedit::codeName(error.code)),
             fromUtf8(error.message));
    if (error.line > 0) text += tr(" (line %1)").arg(error.line);
    if (!error.affectedGroups.empty()) {
        QStringList groups;
        for (const std::string& group : error.affectedGroups) groups << fromUtf8(group);
        text += tr(" [groups: %1]").arg(groups.join(QStringLiteral(", ")));
    }
    appendTrace(QStringLiteral("ERROR ") + text);
    statusBar()->showMessage(text, 15000);
    QMessageBox::critical(this, tr("MeshEditor error"), text);
}

void MainWindow::showBusy() {
    const QString text = tr("E_BUSY: another edit, save, load, export, or validation task is running");
    appendTrace(QStringLiteral("ERROR ") + text);
    statusBar()->showMessage(text, 8000);
}

void MainWindow::setTitle() {
    const QString file = currentPath_.isEmpty() ? tr("Untitled") : QFileInfo(currentPath_).fileName();
    setWindowTitle(tr("MeshEditor - %1 [export v%2]").arg(file).arg(exportVersion_));
}

void MainWindow::commitOutcome(const meshedit::EditOutcome& outcome) {
    document_.commit(outcome);
    view_->setMesh(document_.current, false);
    updateMetrics();
    updateActions();
    appendTrace(tr("EDIT %1: %2\n  before %3\n  after  %4")
        .arg(fromUtf8(outcome.operation), fromUtf8(outcome.detail),
             fromUtf8(meshedit::metricsJson(outcome.beforeMetrics)),
             fromUtf8(meshedit::metricsJson(outcome.afterMetrics))));
    statusBar()->showMessage(
        tr("%1 complete: %2 affected faces")
            .arg(fromUtf8(outcome.operation))
            .arg(outcome.affectedFaceCount),
        8000);
}

void MainWindow::runTask(const QString& label, Work work, std::function<void()> done) {
    if (taskRunning_) {
        showBusy();
        return;
    }
    auto token = std::make_shared<meshedit::CancellationToken>();
    meshedit::TaskGate::Lease lease = taskGate_.acquire(*token);
    if (!lease.active()) {
        showBusy();
        return;
    }
    taskToken_ = std::move(token);
    taskLease_ = std::move(lease);
    taskRunning_ = true;
    updateActions();
    statusBar()->showMessage(tr("%1...").arg(label));

    QPointer<MainWindow> guard(this);
    auto* job = new BackgroundJob([work, token = taskToken_] {
        if (token->cancelled()) return;
        work(token.get());
    });
    connect(job, &BackgroundJob::finished, this, [this, guard, job, done = std::move(done)] {
        if (!guard) return;
        const QString backgroundError = job->errorString();
        if (!backgroundError.isEmpty()) {
            appendTrace(tr("BACKGROUND EXCEPTION: %1").arg(backgroundError));
            view_->releaseGlBuffers();
            statusBar()->showMessage(tr("Background task failed; GPU buffers released"), 10000);
            QMessageBox::critical(this, tr("Background task failed"), backgroundError);
        } else if (done) {
            done();
        }
        if (taskToken_ && taskToken_->cancelled()) {
            appendTrace(tr("E_CANCELLED: worker stopped; transient GPU buffers and file handles are closed"));
            view_->releaseGlBuffers();
        }
        taskRunning_ = false;
        taskLease_.release();
        taskToken_.reset();
        updateActions();
        job->deleteLater();
    });
    pool_.start(job);
}

void MainWindow::cancelTask() {
    if (!taskToken_ || !taskRunning_) return;
    taskToken_->cancel();
    view_->releaseGlBuffers();
    appendTrace(tr("E_CANCELLED requested; GPU buffers released; file handles owned by the job will close"));
    statusBar()->showMessage(tr("Cancellation requested (E_CANCELLED once the worker yields)"), 10000);
}

meshedit::Vec3 MainWindow::selectionPivot() const {
    const meshedit::Selection selection = view_->selection();
    meshedit::Vec3 pivot;
    int count = 0;
    for (int vertex : selection.vertices) {
        if (vertex >= 0 && static_cast<std::size_t>(vertex) < document_.current.positions.size()) {
            pivot += document_.current.positions[static_cast<std::size_t>(vertex)];
            ++count;
        }
    }
    if (count > 0) return pivot / count;
    const auto metrics = document_.metrics();
    return metrics.bounds.valid() ? metrics.bounds.center() : pivot;
}

QString MainWindow::withExtension(QString path, meshedit::MeshFormat format) const {
    const QString suffix = format == meshedit::MeshFormat::Stl
        ? QStringLiteral(".stl") : QStringLiteral(".obj");
    if (path.toLower().endsWith(suffix)) return path;
    const QFileInfo info(path);
    const QString base = info.completeBaseName().isEmpty() ? path : info.completeBaseName();
    return info.absolutePath() + QStringLiteral("/") + base + suffix;
}

QString MainWindow::suggestedExportPath(const QString& suffix) const {
    if (!currentPath_.isEmpty()) {
        const QFileInfo info(currentPath_);
        return info.absolutePath() + QStringLiteral("/") + info.completeBaseName() + suffix;
    }
    return QDir::currentPath() + QStringLiteral("/mesh_export") + suffix;
}

void MainWindow::openMesh() {
    const QString path = QFileDialog::getOpenFileName(
        this, tr("Open surface mesh"), QString(),
        tr("Mesh files (*.obj *.stl);;OBJ (*.obj);;STL (*.stl);;All files (*)"));
    if (path.isEmpty()) return;
    openFilePath(path);
}

void MainWindow::openFilePath(const QString& path) {
    auto result = std::make_shared<meshedit::Result<meshedit::LoadResult>>();
    runTask(tr("Loading %1").arg(QFileInfo(path).fileName()),
        [result, path](meshedit::CancellationToken* token) {
            if (token->cancelled()) return;
            *result = meshedit::loadMesh(path.toStdString());
        },
        [this, result, path] {
            if (taskToken_ && taskToken_->cancelled()) {
                appendTrace(tr("Load cancelled; no partial scene was installed"));
                return;
            }
            if (!result->ok()) {
                view_->releaseGlBuffers();
                displayError(result->error);
                return;
            }
            document_.load(std::move(result->get().mesh));
            currentPath_ = path;
            view_->setMesh(document_.current);
            updateMetrics();
            updateActions();
            setTitle();
            appendTrace(tr("LOAD %1\n%2")
                .arg(QFileInfo(path).fileName(),
                     fromUtf8(result->get().report.toText())));
            if (result->get().report.upgradedOnLoad) {
                appendTrace(tr("Compatibility: v1 file upgraded in memory to v2"));
            }
            statusBar()->showMessage(tr("Loaded %1").arg(QFileInfo(path).fileName()), 8000);
        });
}

void MainWindow::openProject() {
    const QString path = QFileDialog::getOpenFileName(
        this, tr("Open project"), QString(),
        tr("MeshEditor project (*.mep);;All files (*)"));
    if (path.isEmpty()) return;
    auto result = std::make_shared<meshedit::Result<meshedit::ProjectData>>();
    runTask(tr("Opening project"),
        [result, path](meshedit::CancellationToken* token) {
            if (!token->cancelled()) *result = meshedit::ProjectIo::load(path.toStdString());
        },
        [this, result, path] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                appendTrace(tr("Project load rejected; the active scene was not modified"));
                return;
            }
            document_.current = std::move(result->get().current);
            document_.baseline = std::move(result->get().baseline);
            document_.history.undoRecords() = std::move(result->get().undoHistory);
            document_.history.redoRecords() = std::move(result->get().redoHistory);
            exportVersion_ = result->get().exportVersion;
            weldTolerance_ = result->get().weldTolerance;
            validationConfig_ = result->get().validation;
            currentPath_.clear();
            view_->setMesh(document_.current);
            updateMetrics();
            updateActions();
            setTitle();
            appendTrace(tr("PROJECT restored from %1: %2 undo, %3 redo, export v%4")
                .arg(QFileInfo(path).fileName())
                .arg(document_.history.undoCount())
                .arg(document_.history.redoCount())
                .arg(exportVersion_));
        });
}

void MainWindow::saveProject() {
    QString path = QFileDialog::getSaveFileName(
        this, tr("Save project"), currentPath_.isEmpty()
            ? QDir::currentPath() + QStringLiteral("/session.mep") : currentPath_ + QStringLiteral(".mep"),
        tr("MeshEditor project (*.mep)"));
    if (path.isEmpty()) return;
    if (!path.toLower().endsWith(QStringLiteral(".mep"))) path += QStringLiteral(".mep");
    meshedit::ProjectData project;
    project.current = document_.current;
    project.baseline = document_.baseline;
    project.undoHistory = document_.history.undoRecords();
    project.redoHistory = document_.history.redoRecords();
    project.exportVersion = exportVersion_;
    project.weldTolerance = weldTolerance_;
    project.validation = validationConfig_;
    auto result = std::make_shared<meshedit::Result<bool>>();
    runTask(tr("Saving project atomically"),
        [result, path, project = std::move(project)](meshedit::CancellationToken* token) mutable {
            if (!token->cancelled()) {
                *result = meshedit::ProjectIo::saveAtomic(path.toStdString(), project);
            }
        },
        [this, result, path] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                return;
            }
            appendTrace(tr("PROJECT saved atomically: %1").arg(QFileInfo(path).fileName()));
            statusBar()->showMessage(tr("Project saved"), 6000);
        });
}

void MainWindow::exportMesh() {
    ExportDialog dialog(exportVersion_, suggestedExportPath(QStringLiteral(".obj")), this);
    if (dialog.exec() != QDialog::Accepted) return;
    const meshedit::ExportOptions options = dialog.exportOptions();
    QString path = withExtension(dialog.exportPath(), options.format);
    if (path.isEmpty()) return;
    const meshedit::Mesh snapshot = document_.current;
    auto result = std::make_shared<meshedit::Result<meshedit::ExportResult>>();
    runTask(tr("Exporting mesh"),
        [result, snapshot, path, options](meshedit::CancellationToken* token) {
            if (!token->cancelled()) {
                *result = meshedit::exportMesh(snapshot, path.toStdString(), options);
            }
        },
        [this, result, path, options] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                return;
            }
            exportVersion_ = static_cast<int>(options.version);
            setTitle();
            updateActions();
            appendTrace(tr("EXPORT %1\n%2")
                .arg(QFileInfo(path).fileName(), fromUtf8(result->get().report.toText())));
            statusBar()->showMessage(tr("Exported %1").arg(QFileInfo(path).fileName()), 8000);
        });
}

void MainWindow::validateExport() {
    ValidationDialog dialog(suggestedExportPath(QStringLiteral("_validated.obj")), this);
    if (dialog.exec() != QDialog::Accepted) return;
    const meshedit::ExportOptions options = dialog.exportOptions();
    const QString path = withExtension(dialog.exportPath(), options.format);
    const QString reportPath = dialog.reportPath().isEmpty()
        ? path + QStringLiteral(".validation.json") : dialog.reportPath();
    if (path.isEmpty() || reportPath.isEmpty()) return;
    const meshedit::ValidationConfig config = dialog.validationConfig();
    validationConfig_ = config;
    const meshedit::Mesh snapshot = document_.current;
    const meshedit::Mesh truth = document_.baseline;
    auto result = std::make_shared<ValidationJobResult>();
    auto cancelled = std::make_shared<std::atomic<bool>>(false);
    runTask(tr("Exporting and validating"),
        [result, snapshot, truth, path, reportPath, options, config, cancelled]
        (meshedit::CancellationToken* token) {
            auto exportResult = meshedit::exportMesh(snapshot, path.toStdString(), options);
            if (!exportResult.ok()) {
                result->error = exportResult.error;
                return;
            }
            result->conversion = exportResult->report;
            if (token->cancelled()) {
                cancelled->store(true);
                return;
            }
            auto loaded = meshedit::loadMesh(path.toStdString());
            if (!loaded.ok()) {
                result->error = loaded.error;
                return;
            }
            result->candidate = std::move(loaded->mesh);
            if (token->cancelled()) {
                cancelled->store(true);
                return;
            }
            auto validation = meshedit::validateMeshes(truth, result->candidate, config);
            if (!validation.ok()) {
                result->error = validation.error;
                return;
            }
            result->report = std::move(validation.get());
            result->haveReport = true;
            if (token->cancelled()) {
                cancelled->store(true);
                return;
            }
            auto written = meshedit::writeValidationReport(result->report,
                                                            reportPath.toStdString());
            if (!written.ok()) result->error = written.error;
        },
        [this, result, reportPath, cancelled, path] {
            if ((taskToken_ && taskToken_->cancelled()) ||
                cancelled->load()) {
                appendTrace(tr("Validation cancelled; candidate overlay and GPU buffers released"));
                view_->releaseGlBuffers();
                return;
            }
            appendTrace(tr("VALIDATION EXPORT %1\n%2")
                .arg(QFileInfo(path).fileName(), fromUtf8(result->conversion.toText())));
            if (!result->error.ok()) {
                displayError(result->error);
                return;
            }
            if (!result->haveReport) {
                appendTrace(tr("Validation did not produce a report"));
                return;
            }
            appendTrace(fromUtf8(result->report.toText()));
            appendTrace(tr("Report written: %1").arg(reportPath));
            if (result->report.passed) {
                view_->clearValidationOverlay();
                view_->setHighlightFaces({});
                statusBar()->showMessage(tr("Validation PASS"), 12000);
            } else {
                view_->setValidationOverlay(result->candidate,
                                            result->report.violatingCandidateFaces);
                statusBar()->showMessage(
                    tr("Validation FAIL: %1 candidate faces exceed tolerance")
                        .arg(result->report.violatingCandidateFaces.size()), 15000);
            }
        });
}

void MainWindow::weldMesh() {
    WeldDialog dialog(weldTolerance_, this);
    if (dialog.exec() != QDialog::Accepted) return;
    const double tolerance = dialog.tolerance();
    const meshedit::Mesh snapshot = document_.current;
    auto result = std::make_shared<meshedit::Result<meshedit::EditOutcome>>();
    runTask(tr("Welding vertices"),
        [result, snapshot, tolerance](meshedit::CancellationToken* token) {
            if (token->cancelled()) return;
            meshedit::Mesh mesh = snapshot;
            meshedit::EditOutcome outcome;
            outcome.beforeMetrics = meshedit::computeMetrics(mesh);
            auto welded = meshedit::weldMesh(mesh, meshedit::WeldOptions{tolerance, true});
            if (!welded.ok()) {
                *result = welded.error;
                return;
            }
            outcome.mesh = std::move(mesh);
            outcome.afterMetrics = meshedit::computeMetrics(outcome.mesh);
            outcome.operation = "weld";
            outcome.detail = welded->toJson();
            outcome.affectedFaceCount = welded->affectedFaceCount;
            outcome.weldReport = std::move(welded.get());
            *result = std::move(outcome);
        },
        [this, result, tolerance] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                return;
            }
            weldTolerance_ = tolerance;
            commitOutcome(result->get());
            if (result->get().weldReport) {
                appendTrace(tr("WELD REPORT %1").arg(
                    fromUtf8(result->get().weldReport->toJson())));
            }
        });
}

void MainWindow::simplifyMesh() {
    SimplifyDialog dialog(this);
    if (dialog.exec() != QDialog::Accepted) return;
    const meshedit::SimplifyOptions options{dialog.targetRatio()};
    const meshedit::Mesh snapshot = document_.current;
    auto result = std::make_shared<meshedit::Result<meshedit::EditOutcome>>();
    runTask(tr("Simplifying mesh"),
        [result, snapshot, options](meshedit::CancellationToken* token) {
            if (!token->cancelled()) *result = meshedit::applySimplify(snapshot, options);
        },
        [this, result] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                return;
            }
            commitOutcome(result->get());
        });
}

void MainWindow::transformSelection() {
    if (view_->selection().empty()) {
        QMessageBox::information(this, tr("Transform"),
                                 tr("Box-select faces or vertices before transforming."));
        return;
    }
    TransformDialog dialog(selectionPivot(), this);
    if (dialog.exec() != QDialog::Accepted) return;
    const meshedit::TransformSpec transform = dialog.transform();
    const meshedit::Mesh snapshot = document_.current;
    const meshedit::Selection selection = view_->selection();
    auto result = std::make_shared<meshedit::Result<meshedit::EditOutcome>>();
    runTask(tr("Transforming selection"),
        [result, snapshot, selection, transform](meshedit::CancellationToken* token) {
            if (!token->cancelled()) {
                *result = meshedit::applyTransform(snapshot, selection, transform);
            }
        },
        [this, result] {
            if (taskToken_ && taskToken_->cancelled()) return;
            if (!result->ok()) {
                displayError(result->error);
                return;
            }
            view_->clearSelection();
            commitOutcome(result->get());
        });
}

void MainWindow::undo() {
    if (taskRunning_) {
        showBusy();
        return;
    }
    if (!document_.undo()) {
        statusBar()->showMessage(tr("Nothing to undo"), 4000);
        return;
    }
    view_->setMesh(document_.current, false);
    updateMetrics();
    updateActions();
    appendTrace(tr("UNDO; restored geometry metrics exactly"));
}

void MainWindow::redo() {
    if (taskRunning_) {
        showBusy();
        return;
    }
    if (!document_.redo()) {
        statusBar()->showMessage(tr("Nothing to redo"), 4000);
        return;
    }
    view_->setMesh(document_.current, false);
    updateMetrics();
    updateActions();
    appendTrace(tr("REDO; restored geometry metrics exactly"));
}

void MainWindow::clearOverlay() {
    view_->clearValidationOverlay();
    view_->fitView();
    appendTrace(tr("Validation overlay cleared"));
}

void MainWindow::fitView() {
    view_->fitView();
}
