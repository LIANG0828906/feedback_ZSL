#include <QApplication>
#include <QFileInfo>
#include <QLabel>
#include <QMessageBox>
#include <QPixmap>
#include <QString>

int main(int argc, char** argv) {
    QApplication application(argc, argv);
    if (argc != 2) {
        QMessageBox::information(
            nullptr, QStringLiteral("fill_preview"),
            QStringLiteral("Usage: fill_preview <fill_bitmap.ppm>"));
        return 1;
    }
    const QString path = QString::fromLocal8Bit(argv[1]);
    QPixmap image(path);
    if (image.isNull()) {
        QMessageBox::critical(nullptr, QStringLiteral("fill_preview"),
                              QStringLiteral("Cannot load image: ") + path);
        return 2;
    }
    QLabel label;
    label.setWindowTitle(QStringLiteral("fill_preview - ") +
                         QFileInfo(path).fileName());
    label.setAlignment(Qt::AlignCenter);
    const QPixmap scaled = image.scaled(image.size() * 8,
                                        Qt::KeepAspectRatio,
                                        Qt::FastTransformation);
    label.setPixmap(scaled);
    label.resize(scaled.size());
    label.show();
    return application.exec();
}
