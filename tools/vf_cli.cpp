#include "vforensics/forensic_engine.h"
#include "vforensics/forensic_worker.h"
#include "vforensics/report.h"
#include <QCommandLineParser>
#include <QCoreApplication>
#include <QFile>
#include <QTextStream>

using namespace vforensics;

namespace {

int printError(const Error& e) {
  QTextStream(stderr) << statusName(e.code) << ": " << e.message << "\n";
  return static_cast<int>(e.code);
}

int runVerify(const QCommandLineParser& parser, bool resumeMode) {
  const QString workdir = parser.value("workdir");
  const uint64_t clock = parser.value("clock").toULongLong();
  ForensicEngine engine(workdir, clock);
  RunStats stats;
  ForensicResult result;
  const Error e = resumeMode
      ? engine.resume(parser.value("sequence"), parser.value("manifest"),
                      parser.value("reference"), stats, result)
      : engine.verify(parser.value("sequence"), parser.value("manifest"),
                      parser.value("reference"), stats, result);
  if (!e.ok()) return printError(e);

  QTextStream out(stdout);
  out << "resultVersion=" << stats.resultVersion << "\n"
      << "recomputed=" << stats.recomputedFrames << " reused=" << stats.reusedFrames
      << " affected=" << stats.affectedFrames << "\n";
  for (const FrameVerification& fv : result.frames) {
    out << "frame=" << fv.index
        << " verdict=" << (fv.hashMatch ? "MATCH" : "MISMATCH")
        << " offset=" << fv.firstMismatchOffset
        << " absolute=" << fv.firstMismatchAbsolute
        << " cascade=" << (fv.cascadeUntrusted ? "UNTRUSTED" : "TRUSTED")
        << " path=";
    for (size_t i = 0; i < fv.cascadePath.size(); ++i) {
      out << fv.cascadePath[i] << (i + 1 == fv.cascadePath.size() ? "" : ">");
    }
    out << " sha256=" << QString::fromLatin1(fv.contentHash.toHex()) << "\n";
  }
  for (const AnchorDiff& d : result.anchorDiffs) {
    out << "anchorDiff segment=" << d.segmentId << " " << d.change << " " << d.detail << "\n";
  }
  if (parser.isSet("report")) {
    const Error re = DiagnosticReport::exportJson(result, parser.value("report"));
    if (!re.ok()) return printError(re);
  }
  if (parser.isSet("trajectory")) {
    QFile f(parser.value("trajectory"));
    f.open(QIODevice::WriteOnly | QIODevice::Truncate);
    f.write(DiagnosticReport::trajectoryLog(result));
    f.close();
  }
  return 0;
}

} // namespace

int main(int argc, char** argv) {
  QCoreApplication app(argc, argv);
  QCommandLineParser parser;
  parser.setApplicationDescription("Video frame forensics CLI");
  parser.addPositionalArgument("command", "verify | resume | report | busy-test");
  QCommandLineOption seqOpt("sequence", "Frame sequence .vfseq", "path");
  QCommandLineOption manOpt("manifest", "Dependency manifest JSON", "path");
  QCommandLineOption refOpt("reference", "Optional .vref original pixels", "path");
  QCommandLineOption workOpt("workdir", "Snapshot/cache working directory", "dir");
  QCommandLineOption reportOpt("report", "Write JSON diagnostic report", "path");
  QCommandLineOption trajOpt("trajectory", "Write deterministic trajectory log", "path");
  QCommandLineOption clockOpt("clock", "Fixed published-at epoch ms", "ms", "0");
  for (auto* o : {&seqOpt, &manOpt, &refOpt, &workOpt, &reportOpt, &trajOpt, &clockOpt}) {
    parser.addOption(*o);
  }
  parser.addHelpOption();
  parser.process(app);
  const QStringList args = parser.positionalArguments();
  if (args.isEmpty()) parser.showHelp(1);
  const QString cmd = args.first();

  if (cmd == "verify") return runVerify(parser, false);
  if (cmd == "resume") return runVerify(parser, true);
  if (cmd == "report") {
    ForensicWorker worker(parser.value("workdir"));
    ForensicResult result;
    Error e = worker.readPublished(result);
    if (!e.ok()) return printError(e);
    e = DiagnosticReport::exportJson(result, parser.value("report"));
    if (!e.ok()) return printError(e);
    return 0;
  }
  if (cmd == "busy-test") {
    ForensicWorker worker(parser.value("workdir"));
    ForensicWorker::JobRequest req{parser.value("sequence"), parser.value("manifest"),
                                   parser.value("reference"), false};
    Error e1 = worker.submit(req);
    Error e2 = worker.submit(req);
    QTextStream(stdout) << statusName(e1.code) << " then " << statusName(e2.code) << "\n";
    worker.cancel();
    return e2.code == StatusCode::E_BUSY ? 0 : 3;
  }
  QTextStream(stderr) << "unknown command: " << cmd << "\n";
  return 1;
}
