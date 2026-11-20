#include "main_window.h"
#include <QApplication>
#include <QCommandLineParser>

int main(int argc, char** argv) {
  QApplication app(argc, argv);
  QCommandLineParser parser;
  parser.addHelpOption();
  QCommandLineOption workOpt("workdir", "Directory containing published snapshots", "dir", ".");
  QCommandLineOption seqOpt("sequence", "Optional .vfseq for OpenCV frame preview", "path", "");
  parser.addOption(workOpt);
  parser.addOption(seqOpt);
  parser.process(app);
  vforensics::MainWindow window(parser.value(workOpt));
  window.setSequencePath(parser.value(seqOpt));
  window.resize(960, 560);
  window.show();
  return app.exec();
}
