#pragma once

#include "font_atlas/engine.hpp"
#include "font_atlas/types.hpp"

#include <QMainWindow>
#include <QSet>

#include <atomic>
#include <memory>
#include <thread>
#include <vector>

class QComboBox;
class QCloseEvent;
class QDoubleSpinBox;
class QLabel;
class QLineEdit;
class QPlainTextEdit;
class QPushButton;
class QSpinBox;
class PreviewWidget;

class MainWindow : public QMainWindow {
    Q_OBJECT

public:
    explicit MainWindow(QWidget* parent = nullptr);
    ~MainWindow() override;
    QString openGlSummary() const;

protected:
    void closeEvent(QCloseEvent* event) override;

private slots:
    void chooseFont();
    void chooseConfig();
    void startRaster();
    void cancelRaster();

private:
    struct WorkerOutcome {
        bool ok = false;
        std::vector<font_atlas::RasterResult> glyphs;
        font_atlas::Diagnostic error;
        font_atlas::AtlasManifest atlas;
        QString atlasPath;
    };

    void finishWorker(std::shared_ptr<WorkerOutcome> outcome);
    void updateProgress(const font_atlas::ProgressEvent& event);
    void setRunning(bool running);
    std::vector<std::string> resolveTextGlyphs() const;

    PreviewWidget* preview_ = nullptr;
    QLineEdit* fontPath_ = nullptr;
    QLineEdit* configPath_ = nullptr;
    QPlainTextEdit* textInput_ = nullptr;
    QSpinBox* size_ = nullptr;
    QSpinBox* workers_ = nullptr;
    QDoubleSpinBox* offsetX_ = nullptr;
    QDoubleSpinBox* offsetY_ = nullptr;
    QComboBox* fillRule_ = nullptr;
    QPushButton* startButton_ = nullptr;
    QPushButton* cancelButton_ = nullptr;
    QLabel* status_ = nullptr;
    QLabel* resource_ = nullptr;
    QLabel* glStatus_ = nullptr;
    QSet<int> completedChunks_;

    std::unique_ptr<font_atlas::FontEngine> engine_;
    std::thread worker_;
    std::atomic<bool> cancelRequested_{false};
    bool running_ = false;
};

