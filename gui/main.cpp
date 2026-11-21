#include "MainWindow.h"

#include <QApplication>
#include <QSurfaceFormat>
#include <QTimer>

#include <cstdio>
#include <cstring>

int main(int argc, char** argv) {
    QSurfaceFormat format;
    format.setVersion(3, 3);
    format.setProfile(QSurfaceFormat::CoreProfile);
    format.setDepthBufferSize(24);
    format.setStencilBufferSize(8);
    QSurfaceFormat::setDefaultFormat(format);

    QApplication application(argc, argv);
    MainWindow window;
    window.show();
    if (argc > 1 && std::strcmp(argv[1], "--smoke-test") == 0) {
        QTimer::singleShot(1000, &application, [&application, &window]() {
            const QString summary = window.openGlSummary();
            std::fprintf(stdout, "%s\n", summary.toUtf8().constData());
            std::fflush(stdout);
            const bool initialized =
                !summary.contains(QStringLiteral("not initialized"));
            application.exit(initialized ? 0 : 3);
        });
    }
    return application.exec();
}
