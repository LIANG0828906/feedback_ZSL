#pragma once

#include "core/Deviation.h"
#include "core/Editing.h"
#include "core/Format.h"

#include <QDialog>
#include <QString>

class QComboBox;
class QDoubleSpinBox;
class QLineEdit;
class QSpinBox;

class WeldDialog : public QDialog {
    Q_OBJECT
public:
    explicit WeldDialog(double tolerance, QWidget* parent = nullptr);
    double tolerance() const;

private:
    QDoubleSpinBox* tolerance_ = nullptr;
};

class SimplifyDialog : public QDialog {
    Q_OBJECT
public:
    explicit SimplifyDialog(QWidget* parent = nullptr);
    double targetRatio() const;

private:
    QDoubleSpinBox* ratio_ = nullptr;
};

class ExportDialog : public QDialog {
    Q_OBJECT
public:
    explicit ExportDialog(int preferredVersion, const QString& suggestedPath,
                          QWidget* parent = nullptr);
    meshedit::ExportOptions exportOptions() const;
    QString exportPath() const;

private slots:
    void browse();
    void formatChanged();

private:
    QComboBox* format_ = nullptr;
    QComboBox* version_ = nullptr;
    QComboBox* variant_ = nullptr;
    QLineEdit* path_ = nullptr;
};

class TransformDialog : public QDialog {
    Q_OBJECT
public:
    explicit TransformDialog(const meshedit::Vec3& pivot, QWidget* parent = nullptr);
    meshedit::TransformSpec transform() const;

private:
    QDoubleSpinBox* translate_[3] = {nullptr, nullptr, nullptr};
    QDoubleSpinBox* rotateAxis_[3] = {nullptr, nullptr, nullptr};
    QDoubleSpinBox* rotateAngle_ = nullptr;
    QDoubleSpinBox* scale_[3] = {nullptr, nullptr, nullptr};
    QDoubleSpinBox* pivot_[3] = {nullptr, nullptr, nullptr};
};

class ValidationDialog : public QDialog {
    Q_OBJECT
public:
    explicit ValidationDialog(const QString& suggestedPath, QWidget* parent = nullptr);
    meshedit::ExportOptions exportOptions() const;
    meshedit::ValidationConfig validationConfig() const;
    QString exportPath() const;
    QString reportPath() const;

private slots:
    void browseExport();
    void browseReport();
    void formatChanged();

private:
    QComboBox* format_ = nullptr;
    QComboBox* version_ = nullptr;
    QComboBox* variant_ = nullptr;
    QLineEdit* exportPath_ = nullptr;
    QLineEdit* reportPath_ = nullptr;
    QDoubleSpinBox* tolerance_ = nullptr;
    QSpinBox* density_ = nullptr;
    QDoubleSpinBox* normalAngle_ = nullptr;
};
