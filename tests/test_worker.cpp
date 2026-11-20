#include "vforensics/forensic_worker.h"
#include "vforensics/frame_sequence_writer.h"
#include <QDir>
#include <QSignalSpy>
#include <QTest>
#include <QtTest/QtTest>

using namespace vforensics;

class WorkerTest : public QObject {
  Q_OBJECT
  SequenceHeader h{1, 4, 4, 3, 5, 1000, 40000};
private slots:
  void duplicateSubmitReturnsBusyAndCancelKeepsPublished();
};

void WorkerTest::duplicateSubmitReturnsBusyAndCancelKeepsPublished() {
  const QString workdir = QDir::tempPath() + "/vf_worker";
  QDir().mkpath(workdir);
  const QString seq = workdir + "/s.vfseq";
  const QString ref = workdir + "/s.vref";
  const QString man = workdir + "/s.manifest.json";
  std::vector<QByteArray> payloads;
  for (uint32_t i = 0; i < h.frameCount; ++i) payloads.emplace_back(48, char('a' + i));
  QVERIFY(FrameSequenceWriter::write(seq, h, payloads).ok());
  QVERIFY(FrameSequenceWriter::write(ref, h, payloads).ok());

  QJsonObject root;
  root["manifestVersion"] = 1;
  QJsonArray deps;
  for (uint32_t i = 0; i < h.frameCount; ++i) {
    QJsonObject e;
    e["frame"] = static_cast<int>(i);
    QJsonArray refs;
    if (i > 0) refs.append(static_cast<int>(i - 1));
    e["references"] = refs;
    deps.append(e);
  }
  root["dependencies"] = deps;
  QJsonArray anchors;
  QJsonObject a;
  a["segmentId"] = 0; a["startFrame"] = 0; a["endFrame"] = 4;
  a["watermarkX"] = 1; a["watermarkY"] = 1; a["referenceType"] = "KEYFRAME";
  anchors.append(a);
  root["anchors"] = anchors;
  QFile mf(man);
  mf.open(QIODevice::WriteOnly | QIODevice::Truncate);
  mf.write(QJsonDocument(root).toJson());
  mf.close();

  // Publish a baseline result first.
  {
    ForensicEngine engine(workdir, 1700000000100ULL);
    RunStats stats;
    ForensicResult result;
    QVERIFY(engine.verify(seq, man, ref, stats, result).ok());
  }

  ForensicWorker worker(workdir, 1700000000200ULL);
  ForensicWorker::JobRequest req{seq, man, ref, false};
  const Error first = worker.submit(req);
  QVERIFY(first.ok());
  const Error second = worker.submit(req);
  QCOMPARE(second.code, StatusCode::E_BUSY);
  worker.cancel();

  QSignalSpy spy(&worker, &ForensicWorker::finished);
  QTRY_VERIFY_WITH_TIMEOUT(spy.count() >= 1, 5000);

  // UI preview still reads the previously published snapshot, never partial.
  ForensicResult published;
  QVERIFY(worker.readPublished(published).ok());
  QCOMPARE(published.frames.size(), size_t(h.frameCount));
  QVERIFY(!worker.isBusy());
}

QTEST_GUILESS_MAIN(WorkerTest)
#include "test_worker.moc"
