#include "MainWindow.h"

#include "PreviewWidget.h"

#include "font_atlas/atlas.hpp"
#include "font_atlas/io.hpp"
#include "font_atlas/workbench.hpp"

#include <QCloseEvent>
#include <QComboBox>
#include <QDoubleSpinBox>
#include <QFileDialog>
#include <QFormLayout>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QLineEdit>
#include <QMetaObject>
#include <QPlainTextEdit>
#include <QPushButton>
#include <QSpinBox>
#include <QSplitter>
#include <QTimer>
#include <QVBoxLayout>
#include <QWidget>

#include <algorithm>
#include <cstring>
#include <filesystem>

namespace {

QImage coverageImage(const font_atlas::RasterResult& glyph) {
    QImage image(glyph.width, glyph.height, QImage::Format_Grayscale8);
    for (int y = 0; y < glyph.height; ++y) {
        std::memcpy(image.scanLine(y),
                    glyph.coverage.data() +
                        static_cast<std::size_t>(y) * glyph.width,
                    static_cast<std::size_t>(glyph.width));
    }
    return image;
}

std::string glyphForText(const font_atlas::Font& font, const QString& text) {
    const std::string name = text.toStdString();
    if (font.findGlyph(name) != nullptr) {
        return name;
    }
    const QByteArray utf8 = text.toUtf8();
    if (!utf8.isEmpty()) {
        const unsigned char first = static_cast<unsigned char>(utf8[0]);
        std::uint32_t codePoint = first;
        int extra = 0;
        if ((first & 0xe0u) == 0xc0u) {
            codePoint = first & 0x1fu;
            extra = 1;
        } else if ((first & 0xf0u) == 0xe0u) {
            codePoint = first & 0x0fu;
            extra = 2;
        } else if ((first & 0xf8u) == 0xf0u) {
            codePoint = first & 0x07u;
            extra = 3;
        }
        for (int i = 1; i <= extra && i < utf8.size(); ++i) {
            codePoint = (codePoint << 6u) |
                        (static_cast<unsigned char>(utf8[i]) & 0x3fu);
        }
        const auto* glyph = font.findUnicode(codePoint);
        if (glyph != nullptr) {
            return glyph->name;
        }
    }
    return font.glyphOrder.empty() ? std::string() : font.glyphOrder.front();
}

}  // namespace

MainWindow::MainWindow(QWidget* parent) : QMainWindow(parent) {
    setWindowTitle(QStringLiteral("Font Atlas Workbench"));
    resize(1180, 760);

    auto* central = new QWidget(this);
    auto* root = new QHBoxLayout(central);
    auto* splitter = new QSplitter(Qt::Horizontal, central);
    preview_ = new PreviewWidget(splitter);
    splitter->addWidget(preview_);

    auto* controls = new QWidget(splitter);
    auto* layout = new QVBoxLayout(controls);

    auto* assetsGroup = new QGroupBox(QStringLiteral("Local assets"), controls);
    auto* assetsForm = new QFormLayout(assetsGroup);
    fontPath_ = new QLineEdit(assetsGroup);
    fontPath_->setPlaceholderText(QStringLiteral("samples/font.json"));
    configPath_ = new QLineEdit(assetsGroup);
    configPath_->setPlaceholderText(QStringLiteral("samples/config.json"));
    auto* fontRow = new QWidget(assetsGroup);
    auto* fontRowLayout = new QHBoxLayout(fontRow);
    fontRowLayout->setContentsMargins(0, 0, 0, 0);
    fontRowLayout->addWidget(fontPath_);
    auto* fontBrowse = new QPushButton(QStringLiteral("..."), fontRow);
    fontRowLayout->addWidget(fontBrowse);
    auto* configRow = new QWidget(assetsGroup);
    auto* configRowLayout = new QHBoxLayout(configRow);
    configRowLayout->setContentsMargins(0, 0, 0, 0);
    configRowLayout->addWidget(configPath_);
    auto* configBrowse = new QPushButton(QStringLiteral("..."), configRow);
    configRowLayout->addWidget(configBrowse);
    assetsForm->addRow(QStringLiteral("Font JSON"), fontRow);
    assetsForm->addRow(QStringLiteral("Config JSON"), configRow);
    layout->addWidget(assetsGroup);

    auto* rasterGroup = new QGroupBox(QStringLiteral("Raster settings"), controls);
    auto* rasterForm = new QFormLayout(rasterGroup);
    textInput_ = new QPlainTextEdit(rasterGroup);
    textInput_->setPlainText(QStringLiteral("AO."));
    textInput_->setMaximumHeight(70);
    size_ = new QSpinBox(rasterGroup);
    size_->setRange(1, 8192);
    size_->setValue(48);
    workers_ = new QSpinBox(rasterGroup);
    workers_->setRange(1, 256);
    workers_->setValue(4);
    offsetX_ = new QDoubleSpinBox(rasterGroup);
    offsetX_->setRange(-1.0, 1.0);
    offsetX_->setDecimals(3);
    offsetX_->setSingleStep(0.125);
    offsetX_->setValue(0.25);
    offsetY_ = new QDoubleSpinBox(rasterGroup);
    offsetY_->setRange(-1.0, 1.0);
    offsetY_->setDecimals(3);
    offsetY_->setSingleStep(0.125);
    offsetY_->setValue(0.125);
    fillRule_ = new QComboBox(rasterGroup);
    fillRule_->addItem(QStringLiteral("nonzero"), QStringLiteral("nonzero"));
    fillRule_->addItem(QStringLiteral("evenodd"), QStringLiteral("evenodd"));
    rasterForm->addRow(QStringLiteral("Text / glyphs"), textInput_);
    rasterForm->addRow(QStringLiteral("Pixel size"), size_);
    rasterForm->addRow(QStringLiteral("Workers"), workers_);
    rasterForm->addRow(QStringLiteral("Subpixel X"), offsetX_);
    rasterForm->addRow(QStringLiteral("Subpixel Y"), offsetY_);
    rasterForm->addRow(QStringLiteral("Fill rule"), fillRule_);
    layout->addWidget(rasterGroup);

    auto* actionRow = new QHBoxLayout();
    startButton_ = new QPushButton(QStringLiteral("Start"), controls);
    cancelButton_ = new QPushButton(QStringLiteral("Cancel"), controls);
    cancelButton_->setEnabled(false);
    actionRow->addWidget(startButton_);
    actionRow->addWidget(cancelButton_);
    layout->addLayout(actionRow);

    status_ = new QLabel(QStringLiteral("Idle"), controls);
    resource_ = new QLabel(QStringLiteral("Chunks: 0    Active: 0 B    Peak: 0 B"),
                           controls);
    glStatus_ = new QLabel(preview_->openGlSummary(), controls);
    for (QLabel* label : {status_, resource_, glStatus_}) {
        label->setWordWrap(true);
        layout->addWidget(label);
    }
    layout->addStretch(1);

    splitter->addWidget(controls);
    splitter->setStretchFactor(0, 4);
    splitter->setStretchFactor(1, 2);
    root->addWidget(splitter);
    setCentralWidget(central);

    connect(fontBrowse, &QPushButton::clicked, this, &MainWindow::chooseFont);
    connect(configBrowse, &QPushButton::clicked, this, &MainWindow::chooseConfig);
    connect(startButton_, &QPushButton::clicked, this, &MainWindow::startRaster);
    connect(cancelButton_, &QPushButton::clicked, this, &MainWindow::cancelRaster);
    QTimer::singleShot(250, this, [this]() {
        glStatus_->setText(preview_->openGlSummary());
    });
}

MainWindow::~MainWindow() {
    cancelRequested_.store(true);
    if (worker_.joinable()) {
        worker_.join();
    }
}

QString MainWindow::openGlSummary() const {
    return preview_->openGlSummary();
}

void MainWindow::closeEvent(QCloseEvent* event) {
    cancelRequested_.store(true);
    if (worker_.joinable()) {
        worker_.join();
    }
    event->accept();
}

void MainWindow::chooseFont() {
    const QString path = QFileDialog::getOpenFileName(
        this, QStringLiteral("Font JSON"), QString(), QStringLiteral("JSON (*.json)"));
    if (!path.isEmpty()) {
        fontPath_->setText(path);
    }
}

void MainWindow::chooseConfig() {
    const QString path = QFileDialog::getOpenFileName(
        this, QStringLiteral("Config JSON"), QString(), QStringLiteral("JSON (*.json)"));
    if (!path.isEmpty()) {
        configPath_->setText(path);
    }
}

std::vector<std::string> MainWindow::resolveTextGlyphs() const {
    if (!engine_) {
        return {};
    }
    std::vector<std::string> glyphs;
    const QString text = textInput_->toPlainText();
    for (const QChar character : text) {
        if (character.isSpace()) {
            continue;
        }
        const std::string glyph =
            glyphForText(engine_->font(), QString(character));
        if (!glyph.empty() &&
            std::find(glyphs.begin(), glyphs.end(), glyph) == glyphs.end()) {
            glyphs.push_back(glyph);
        }
    }
    if (glyphs.empty() && !engine_->font().glyphOrder.empty()) {
        glyphs.push_back(engine_->font().glyphOrder.front());
    }
    return glyphs;
}

void MainWindow::startRaster() {
    if (running_) {
        return;
    }
    if (fontPath_->text().isEmpty() || configPath_->text().isEmpty()) {
        status_->setText(QStringLiteral("Choose local font and config JSON files first."));
        return;
    }
    auto font = font_atlas::loadFontFile(fontPath_->text().toStdString());
    if (!font.ok()) {
        status_->setText(QString::fromStdString(
            std::string(font_atlas::errorCodeName(font.error.code)) + ": " +
            font.error.message));
        return;
    }
    auto config =
        font_atlas::loadWorkbenchConfig(configPath_->text().toStdString());
    if (!config.ok()) {
        status_->setText(QString::fromStdString(config.error.message));
        return;
    }
    config.value->raster.targetPixelSize = size_->value();
    config.value->raster.subpixelX = offsetX_->value();
    config.value->raster.subpixelY = offsetY_->value();
    config.value->budget.parallelism = workers_->value();
    font_atlas::parseFillRule(fillRule_->currentData().toString().toStdString(),
                              config.value->raster.fillRule);

    engine_ = std::make_unique<font_atlas::FontEngine>(std::move(*font.value));
    const std::vector<std::string> glyphs = resolveTextGlyphs();
    if (glyphs.empty()) {
        status_->setText(QStringLiteral("No rasterizable glyphs in the text input."));
        return;
    }
    cancelRequested_.store(false);
    completedChunks_.clear();
    preview_->clearPreview();
    setRunning(true);
    status_->setText(QStringLiteral("Rasterizing %1 glyph(s)...")
                         .arg(static_cast<int>(glyphs.size())));

    const std::filesystem::path output =
        std::filesystem::path("workbench_out");
    const std::filesystem::path state = output / "state";
    const font_atlas::WorkbenchConfig workingConfig = *config.value;
    auto outcome = std::make_shared<WorkerOutcome>();
    worker_ = std::thread([this, glyphs, workingConfig, output, state, outcome]() {
        for (std::size_t i = 0; i < glyphs.size(); ++i) {
            if (cancelRequested_.load()) {
                outcome->error.code = font_atlas::ErrorCode::Cancelled;
                outcome->error.message = "cancelled by user";
                break;
            }
            font_atlas::RasterRequest request;
            request.glyph = glyphs[i];
            request.raster = workingConfig.raster;
            request.budget = workingConfig.budget;
            request.persist = true;
            request.resume = false;
            request.stateDirectory =
                (state / glyphs[i]).string();
            auto result = engine_->rasterize(
                request, [this]() { return cancelRequested_.load(); },
                [this](const font_atlas::ProgressEvent& event) {
                    QMetaObject::invokeMethod(
                        this, [this, event]() { updateProgress(event); },
                        Qt::QueuedConnection);
                });
            if (!result.ok()) {
                outcome->error = std::move(result.error);
                break;
            }
            outcome->glyphs.push_back(std::move(*result.value));
        }
        if (outcome->error.ok() && !cancelRequested_.load()) {
            auto atlas = font_atlas::packAtlas(outcome->glyphs,
                                               workingConfig.atlas, output);
            if (atlas.ok()) {
                outcome->ok = true;
                outcome->atlas = std::move(*atlas.value);
                outcome->atlasPath =
                    QString::fromStdString((output / "atlas.pgm").string());
            } else {
                outcome->error = std::move(atlas.error);
            }
        }
        if (!outcome->error.ok() &&
            outcome->error.code == font_atlas::ErrorCode::Cancelled) {
            outcome->ok = false;
        }
        QMetaObject::invokeMethod(
            this, [this, outcome]() { finishWorker(outcome); },
            Qt::QueuedConnection);
    });
}

void MainWindow::cancelRaster() {
    if (running_) {
        cancelRequested_.store(true);
        status_->setText(
            QStringLiteral("Cancel requested; waiting for chunk checkpoints..."));
    }
}

void MainWindow::updateProgress(const font_atlas::ProgressEvent& event) {
    resource_->setText(QStringLiteral(
                           "Chunks: %1 / %2    Active: %3 B    Peak: %4 B")
                           .arg(event.completed)
                           .arg(event.total)
                           .arg(static_cast<qulonglong>(event.activeBytes))
                           .arg(static_cast<qulonglong>(event.peakBytes)));
    if (event.kind == font_atlas::ProgressKind::ChunkCompleted) {
        completedChunks_.insert(event.chunkIndex);
        preview_->setCompletedChunks(completedChunks_);
    }
    if (event.kind == font_atlas::ProgressKind::Planned) {
        completedChunks_.clear();
        preview_->setCompletedChunks(completedChunks_);
    }
    if (event.kind == font_atlas::ProgressKind::ChunkStarted) {
        status_->setText(QStringLiteral("Rasterizing %1, chunk %2/%3")
                             .arg(QString::fromStdString(event.glyph))
                             .arg(event.chunkIndex)
                             .arg(event.total));
    }
}

void MainWindow::finishWorker(std::shared_ptr<WorkerOutcome> outcome) {
    if (worker_.joinable()) {
        worker_.join();
    }
    setRunning(false);
    if (outcome->ok && !outcome->glyphs.empty()) {
        const auto& glyph = outcome->glyphs.back();
        QImage image = coverageImage(glyph);
        preview_->setGlyphPreview(image, glyph.chunks);
        QSet<int> completed;
        for (const auto& chunk : glyph.chunks) {
            completed.insert(chunk.index);
        }
        preview_->setCompletedChunks(completed);
        status_->setText(
            QStringLiteral("Published atlas: %1 (%2 glyphs)")
                .arg(outcome->atlasPath)
                .arg(static_cast<int>(outcome->atlas.glyphs.size())));
    } else if (outcome->error.code == font_atlas::ErrorCode::Cancelled) {
        status_->setText(
            QStringLiteral("Cancelled. The last successfully published atlas "
                           "was preserved."));
    } else {
        status_->setText(QString::fromStdString(
            std::string(font_atlas::errorCodeName(outcome->error.code)) + ": " +
            outcome->error.message));
    }
}

void MainWindow::setRunning(bool running) {
    running_ = running;
    startButton_->setEnabled(!running);
    cancelButton_->setEnabled(running);
    fontPath_->setEnabled(!running);
    configPath_->setEnabled(!running);
}

