#include "MainWindow.h"
#include "MeshView.h"

#include <QApplication>
#include <QTimer>
#include <QSurfaceFormat>

int main(int argc, char** argv) {
    QSurfaceFormat format;
    format.setVersion(3, 3);
    format.setProfile(QSurfaceFormat::CoreProfile);
    format.setDepthBufferSize(24);
    format.setStencilBufferSize(8);
    format.setSamples(4);
    QSurfaceFormat::setDefaultFormat(format);

    QApplication application(argc, argv);
    application.setApplicationName(QStringLiteral("MeshEditor"));
    application.setOrganizationName(QStringLiteral("MeshEditor"));
    MainWindow window;
    window.show();
    if (application.arguments().size() >= 2 &&
        !application.arguments().at(1).startsWith(QStringLiteral("--"))) {
        window.openFilePath(application.arguments().at(1));
    }
    if (application.arguments().contains(QStringLiteral("--smoke-test"))) {
        const QString output = application.arguments().value(
            application.arguments().indexOf(QStringLiteral("--smoke-test")) + 1,
            QStringLiteral("mesheditor_smoke.png"));
        QTimer::singleShot(3000, &application, [&window, output] {
            const QPixmap screenshot = window.grab();
            screenshot.save(output);
            if (auto* view = window.findChild<MeshView*>()) {
                view->grabFramebuffer().save(output + QStringLiteral(".gl.png"));
            }
            QApplication::quit();
        });
    }
    return application.exec();
}
