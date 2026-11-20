#include "Dialogs.h"

#include "core/Mesh.h"

#include <QComboBox>
#include <QDialogButtonBox>
#include <QDoubleSpinBox>
#include <QFileDialog>
#include <QFormLayout>
#include <QGroupBox>
#include <QHBoxLayout>
#include <QLabel>
#include <QLineEdit>
#include <QPushButton>
#include <QSpinBox>
#include <QVBoxLayout>

namespace {

QDoubleSpinBox* number(double minimum, double maximum, double value, int decimals = 6) {
    auto* box = new QDoubleSpinBox;
    box->setRange(minimum, maximum);
    box->setDecimals(decimals);
    box->setValue(value);
    box->setKeyboardTracking(false);
    return box;
}

} // namespace

WeldDialog::WeldDialog(double tolerance, QWidget* parent) : QDialog(parent) {
    setWindowTitle(tr("Weld vertices"));
    auto* layout = new QVBoxLayout(this);
    auto* info = new QLabel(
        tr("Merges positions within the geometric tolerance, removes degenerated "
           "triangles, recomputes smooth normals, and rebuilds boundary loops."),
        this);
    info->setWordWrap(true);
    layout->addWidget(info);
    auto* form = new QFormLayout;
    tolerance_ = number(0.0, 1e9, tolerance, 9);
    tolerance_->setSingleStep(1e-6);
    form->addRow(tr("Tolerance"), tolerance_);
    layout->addLayout(form);
    auto* buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    layout->addWidget(buttons);
}

double WeldDialog::tolerance() const {
    return tolerance_->value();
}

SimplifyDialog::SimplifyDialog(QWidget* parent) : QDialog(parent) {
    setWindowTitle(tr("Simplify mesh"));
    auto* layout = new QVBoxLayout(this);
    auto* info = new QLabel(
        tr("Deterministic shortest-edge collapse. Degenerate and duplicate triangles "
           "are removed; the operation is single-step undoable."),
        this);
    info->setWordWrap(true);
    layout->addWidget(info);
    auto* form = new QFormLayout;
    ratio_ = number(0.05, 0.99, 0.75, 2);
    ratio_->setSingleStep(0.05);
    ratio_->setSuffix(tr(" x faces"));
    form->addRow(tr("Keep face ratio"), ratio_);
    layout->addLayout(form);
    auto* buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    layout->addWidget(buttons);
}

double SimplifyDialog::targetRatio() const {
    return ratio_->value();
}

ExportDialog::ExportDialog(int preferredVersion, const QString& suggestedPath,
                           QWidget* parent)
    : QDialog(parent) {
    setWindowTitle(tr("Export mesh"));
    auto* layout = new QVBoxLayout(this);
    auto* form = new QFormLayout;
    format_ = new QComboBox(this);
    format_->addItem(tr("OBJ"), QStringLiteral("obj"));
    format_->addItem(tr("STL"), QStringLiteral("stl"));
    form->addRow(tr("Format"), format_);
    version_ = new QComboBox(this);
    version_->addItem(tr("v2 (per-corner attributes)"), 2);
    version_->addItem(tr("v1 (vertex-bound attributes)"), 1);
    version_->setCurrentIndex(preferredVersion == 1 ? 1 : 0);
    form->addRow(tr("OBJ version"), version_);
    variant_ = new QComboBox(this);
    variant_->addItem(tr("Binary"), QStringLiteral("binary"));
    variant_->addItem(tr("ASCII"), QStringLiteral("ascii"));
    form->addRow(tr("STL variant"), variant_);
    path_ = new QLineEdit(suggestedPath, this);
    auto* row = new QHBoxLayout;
    row->addWidget(path_);
    auto* browseButton = new QPushButton(tr("Browse..."), this);
    row->addWidget(browseButton);
    form->addRow(tr("Output file"), row);
    layout->addLayout(form);
    auto* buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(browseButton, &QPushButton::clicked, this, &ExportDialog::browse);
    connect(format_, &QComboBox::currentTextChanged, this,
            [this](const QString&) { formatChanged(); });
    formatChanged();
    layout->addWidget(buttons);
}

meshedit::ExportOptions ExportDialog::exportOptions() const {
    meshedit::ExportOptions options;
    options.format = format_->currentData().toString() == QStringLiteral("stl")
        ? meshedit::MeshFormat::Stl : meshedit::MeshFormat::Obj;
    options.version = version_->currentData().toInt() == 1
        ? meshedit::Version::V1 : meshedit::Version::V2;
    options.stlVariant = variant_->currentData().toString() == QStringLiteral("ascii")
        ? meshedit::StlVariant::Ascii : meshedit::StlVariant::Binary;
    return options;
}

QString ExportDialog::exportPath() const {
    return path_->text().trimmed();
}

void ExportDialog::browse() {
    const QString filter = format_->currentData().toString() == QStringLiteral("stl")
        ? tr("STL mesh (*.stl)") : tr("OBJ mesh (*.obj)");
    const QString path = QFileDialog::getSaveFileName(this, tr("Export mesh"),
                                                       path_->text(), filter);
    if (!path.isEmpty()) path_->setText(path);
}

void ExportDialog::formatChanged() {
    const bool stl = format_->currentData().toString() == QStringLiteral("stl");
    version_->setEnabled(!stl);
    variant_->setEnabled(stl);
}

TransformDialog::TransformDialog(const meshedit::Vec3& pivot, QWidget* parent)
    : QDialog(parent) {
    setWindowTitle(tr("Transform selection"));
    auto* layout = new QVBoxLayout(this);
    auto* info = new QLabel(
        tr("Operations are applied in this order around the pivot: scale, rotate, "
           "translate. Select faces or vertices in the viewport first."),
        this);
    info->setWordWrap(true);
    layout->addWidget(info);

    auto* translationGroup = new QGroupBox(tr("Translation"), this);
    auto* translationLayout = new QHBoxLayout(translationGroup);
    for (int i = 0; i < 3; ++i) {
        translate_[i] = number(-1e9, 1e9, 0.0);
        translationLayout->addWidget(translate_[i]);
    }
    layout->addWidget(translationGroup);

    auto* rotationGroup = new QGroupBox(tr("Rotation"), this);
    auto* rotationLayout = new QHBoxLayout(rotationGroup);
    rotationLayout->addWidget(new QLabel(tr("axis"), rotationGroup));
    for (int i = 0; i < 3; ++i) {
        rotateAxis_[i] = number(-1.0, 1.0, i == 2 ? 1.0 : 0.0, 4);
        rotationLayout->addWidget(rotateAxis_[i]);
    }
    rotateAngle_ = number(-3600.0, 3600.0, 0.0, 3);
    rotateAngle_->setSuffix(tr(" deg"));
    rotationLayout->addWidget(rotateAngle_);
    layout->addWidget(rotationGroup);

    auto* scaleGroup = new QGroupBox(tr("Scale"), this);
    auto* scaleLayout = new QHBoxLayout(scaleGroup);
    for (int i = 0; i < 3; ++i) {
        scale_[i] = number(-1000.0, 1000.0, 1.0, 6);
        scaleLayout->addWidget(scale_[i]);
    }
    layout->addWidget(scaleGroup);

    auto* pivotGroup = new QGroupBox(tr("Pivot"), this);
    auto* pivotLayout = new QHBoxLayout(pivotGroup);
    const double values[3] = {pivot.x, pivot.y, pivot.z};
    for (int i = 0; i < 3; ++i) {
        pivot_[i] = number(-1e9, 1e9, values[i]);
        pivotLayout->addWidget(pivot_[i]);
    }
    layout->addWidget(pivotGroup);

    auto* buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    layout->addWidget(buttons);
}

meshedit::TransformSpec TransformDialog::transform() const {
    meshedit::TransformSpec result;
    result.translation = {translate_[0]->value(), translate_[1]->value(),
                          translate_[2]->value()};
    result.rotationAxis = {rotateAxis_[0]->value(), rotateAxis_[1]->value(),
                           rotateAxis_[2]->value()};
    result.rotationDegrees = rotateAngle_->value();
    result.scale = {scale_[0]->value(), scale_[1]->value(), scale_[2]->value()};
    result.pivot = {pivot_[0]->value(), pivot_[1]->value(), pivot_[2]->value()};
    return result;
}

ValidationDialog::ValidationDialog(const QString& suggestedPath, QWidget* parent)
    : QDialog(parent) {
    setWindowTitle(tr("Export and validate"));
    auto* layout = new QVBoxLayout(this);
    auto* info = new QLabel(
        tr("Exports the current mesh, reloads it, then measures bidirectional "
           "nearest-point deviation and normal angles against the original load."),
        this);
    info->setWordWrap(true);
    layout->addWidget(info);

    auto* form = new QFormLayout;
    format_ = new QComboBox(this);
    format_->addItem(tr("OBJ"), QStringLiteral("obj"));
    format_->addItem(tr("STL"), QStringLiteral("stl"));
    form->addRow(tr("Export format"), format_);
    version_ = new QComboBox(this);
    version_->addItem(tr("v2 (per-corner attributes)"), 2);
    version_->addItem(tr("v1 (vertex-bound attributes)"), 1);
    form->addRow(tr("OBJ version"), version_);
    variant_ = new QComboBox(this);
    variant_->addItem(tr("Binary"), QStringLiteral("binary"));
    variant_->addItem(tr("ASCII"), QStringLiteral("ascii"));
    form->addRow(tr("STL variant"), variant_);

    exportPath_ = new QLineEdit(suggestedPath, this);
    auto* exportRow = new QHBoxLayout;
    exportRow->addWidget(exportPath_);
    auto* exportBrowse = new QPushButton(tr("Browse..."), this);
    exportRow->addWidget(exportBrowse);
    form->addRow(tr("Export file"), exportRow);

    tolerance_ = number(0.0, 1e9, 1e-4, 9);
    tolerance_->setSingleStep(1e-5);
    form->addRow(tr("Distance tolerance"), tolerance_);
    density_ = new QSpinBox(this);
    density_->setRange(1, 64);
    density_->setValue(4);
    form->addRow(tr("Sampling density"), density_);
    normalAngle_ = number(0.0, 180.0, 45.0, 3);
    normalAngle_->setSuffix(tr(" deg"));
    form->addRow(tr("Normal angle limit"), normalAngle_);

    reportPath_ = new QLineEdit(suggestedPath + QStringLiteral(".validation.json"), this);
    auto* reportRow = new QHBoxLayout;
    reportRow->addWidget(reportPath_);
    auto* reportBrowse = new QPushButton(tr("Browse..."), this);
    reportRow->addWidget(reportBrowse);
    form->addRow(tr("Report file"), reportRow);
    layout->addLayout(form);

    auto* buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel, this);
    connect(buttons, &QDialogButtonBox::accepted, this, &QDialog::accept);
    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(exportBrowse, &QPushButton::clicked, this, &ValidationDialog::browseExport);
    connect(reportBrowse, &QPushButton::clicked, this, &ValidationDialog::browseReport);
    connect(format_, &QComboBox::currentTextChanged, this,
            [this](const QString&) { formatChanged(); });
    formatChanged();
    layout->addWidget(buttons);
}

meshedit::ExportOptions ValidationDialog::exportOptions() const {
    meshedit::ExportOptions options;
    options.format = format_->currentData().toString() == QStringLiteral("stl")
        ? meshedit::MeshFormat::Stl : meshedit::MeshFormat::Obj;
    options.version = version_->currentData().toInt() == 1
        ? meshedit::Version::V1 : meshedit::Version::V2;
    options.stlVariant = variant_->currentData().toString() == QStringLiteral("ascii")
        ? meshedit::StlVariant::Ascii : meshedit::StlVariant::Binary;
    return options;
}

meshedit::ValidationConfig ValidationDialog::validationConfig() const {
    meshedit::ValidationConfig config;
    config.distanceTolerance = tolerance_->value();
    config.samplingDensity = density_->value();
    config.normalAngleToleranceDegrees = normalAngle_->value();
    return config;
}

QString ValidationDialog::exportPath() const {
    return exportPath_->text().trimmed();
}

QString ValidationDialog::reportPath() const {
    return reportPath_->text().trimmed();
}

void ValidationDialog::browseExport() {
    const QString filter = format_->currentData().toString() == QStringLiteral("stl")
        ? tr("STL mesh (*.stl)") : tr("OBJ mesh (*.obj)");
    const QString path = QFileDialog::getSaveFileName(this, tr("Export mesh"),
                                                       exportPath_->text(), filter);
    if (!path.isEmpty()) {
        exportPath_->setText(path);
        if (reportPath_->text().isEmpty()) {
            reportPath_->setText(path + QStringLiteral(".validation.json"));
        }
    }
}

void ValidationDialog::browseReport() {
    const QString path = QFileDialog::getSaveFileName(
        this, tr("Validation report"), reportPath_->text(),
        tr("JSON report (*.json)"));
    if (!path.isEmpty()) reportPath_->setText(path);
}

void ValidationDialog::formatChanged() {
    const bool stl = format_->currentData().toString() == QStringLiteral("stl");
    version_->setEnabled(!stl);
    variant_->setEnabled(stl);
}
