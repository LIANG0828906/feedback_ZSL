#pragma once

#include "core/Deviation.h"
#include "core/Editing.h"
#include "core/Format.h"
#include "core/Project.h"
#include "core/TaskGate.h"

#include <QMainWindow>
#include <QThreadPool>

#include <functional>
#include <memory>

class QAction;
class QLabel;
class QPlainTextEdit;
class MeshView;

class MainWindow : public QMainWindow {
    Q_OBJECT
public:
    explicit MainWindow(QWidget* parent = nullptr);
    ~MainWindow() override;
    void openFilePath(const QString& path);

private:
    void createActions();
    void createMenus();
    void createDocks();
    void updateActions();
    void updateMetrics();
    void appendTrace(const QString& text);
    void displayError(const meshedit::Error& error);
    void showBusy();
    void setTitle();
    void commitOutcome(const meshedit::EditOutcome& outcome);
    meshedit::Vec3 selectionPivot() const;
    QString suggestedExportPath(const QString& suffix) const;
    QString withExtension(QString path, meshedit::MeshFormat format) const;

    using Work = std::function<void(meshedit::CancellationToken*)>;
    void runTask(const QString& label, Work work, std::function<void()> done);
    void cancelTask();

    void openMesh();
    void openProject();
    void saveProject();
    void exportMesh();
    void validateExport();
    void weldMesh();
    void simplifyMesh();
    void transformSelection();
    void undo();
    void redo();
    void clearOverlay();
    void fitView();

    MeshView* view_ = nullptr;
    QPlainTextEdit* traceView_ = nullptr;
    QLabel* metricsLabel_ = nullptr;
    QLabel* selectionLabel_ = nullptr;
    QLabel* overlayLabel_ = nullptr;

    QAction* openAction_ = nullptr;
    QAction* openProjectAction_ = nullptr;
    QAction* saveProjectAction_ = nullptr;
    QAction* exportAction_ = nullptr;
    QAction* validateAction_ = nullptr;
    QAction* undoAction_ = nullptr;
    QAction* redoAction_ = nullptr;
    QAction* weldAction_ = nullptr;
    QAction* simplifyAction_ = nullptr;
    QAction* transformAction_ = nullptr;
    QAction* cancelAction_ = nullptr;
    QAction* clearOverlayAction_ = nullptr;

    meshedit::MeshDocument document_;
    QString currentPath_;
    int exportVersion_ = 2;
    double weldTolerance_ = 1e-6;
    meshedit::ValidationConfig validationConfig_;

    meshedit::TaskGate taskGate_;
    meshedit::TaskGate::Lease taskLease_;
    std::shared_ptr<meshedit::CancellationToken> taskToken_;
    QThreadPool pool_;
    bool taskRunning_ = false;
};
