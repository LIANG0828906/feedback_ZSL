#include "vforensics/forensic_engine.h"
#include "vforensics/frame_sequence.h"
#include "vforensics/frame_sequence_writer.h"
#include "vforensics/report.h"
#include <QDir>
#include <QFile>
#include <QJsonDocument>
#include <QtTest/QtTest>

using namespace vforensics;

class IncrementalSnapshotTest : public QObject {
  Q_OBJECT
  SequenceHeader h{1, 4, 4, 3, 6, 1000, 40000};
  QString workdir;
  QString seqPath, refPath, manifestPath;

  void writeManifestV(uint32_t version);
  void makeSamples(bool tamper);
private slots:
  void init();
  void firstRunRecomputesSecondRunReuses();
  void tamperedRunCascadesAndReports();
  void snapshotRecoveryAndBrokenChain();
};

void IncrementalSnapshotTest::init() {
  workdir = QDir::tempPath() + "/vf_inc_" + QString::number(reinterpret_cast<qintptr>(this));
  QDir().mkpath(workdir);
  seqPath = workdir + "/sample.vfseq";
  refPath = workdir + "/sample.vref";
  manifestPath = workdir + "/sample.manifest.json";
}

void IncrementalSnapshotTest::writeManifestV(uint32_t version) {
  QJsonObject root;
  root["manifestVersion"] = static_cast<int>(version);
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
  a["segmentId"] = 0;
  a["startFrame"] = 0;
  a["endFrame"] = static_cast<int>(h.frameCount - 1);
  a["watermarkX"] = 2;
  a["watermarkY"] = 2;
  a["referenceType"] = version == 1 ? "KEYFRAME" : "KEYFRAME_WATERMARKED";
  anchors.append(a);
  root["anchors"] = anchors;
  QFile f(manifestPath);
  f.open(QIODevice::WriteOnly | QIODevice::Truncate);
  f.write(QJsonDocument(root).toJson());
  f.close();
}

void IncrementalSnapshotTest::makeSamples(bool tamper) {
  std::vector<QByteArray> original;
  for (uint32_t i = 0; i < h.frameCount; ++i)
    original.emplace_back(QByteArray(4 * 4 * 3, static_cast<char>(0x30 + i)));
  std::vector<QByteArray> tampered = original;
  if (tamper) tampered[2][5] = char(0x7F);
  std::vector<QByteArray> checksums;
  for (const QByteArray& p : original) checksums.push_back(FrameSequence::sha256(p));
  QVERIFY(FrameSequenceWriter::write(seqPath, h, tampered, tamper ? &checksums : nullptr).ok());
  QVERIFY(FrameSequenceWriter::write(refPath, h, original).ok());
}

void IncrementalSnapshotTest::firstRunRecomputesSecondRunReuses() {
  makeSamples(false);
  writeManifestV(1);
  ForensicEngine engine(workdir, 1700000000000ULL);
  RunStats s1, s2;
  ForensicResult r1, r2;
  QVERIFY2(engine.verify(seqPath, manifestPath, refPath, s1, r1).ok(), "first verify");
  QCOMPARE(s1.recomputedFrames, h.frameCount);
  QVERIFY2(engine.verify(seqPath, manifestPath, refPath, s2, r2).ok(), "second verify");
  QCOMPARE(s2.reusedFrames, h.frameCount);
  QCOMPARE(s2.recomputedFrames, 0u);
  QCOMPARE(r1.frames[0].contentHash, r2.frames[0].contentHash);
  QCOMPARE(r2.resultVersion, 2u);
}

void IncrementalSnapshotTest::tamperedRunCascadesAndReports() {
  makeSamples(true);
  writeManifestV(1);
  ForensicEngine engine(workdir, 1700000000001ULL);
  RunStats stats;
  ForensicResult result;
  const Error e = engine.verify(seqPath, manifestPath, refPath, stats, result);
  QVERIFY2(e.ok(), e.message.toLocal8Bit());
  QCOMPARE(result.trustOf(2), FrameTrust::HashMismatch);
  QCOMPARE(result.frames[2].firstMismatchOffset, 5);
  for (uint32_t i = 3; i < h.frameCount; ++i) {
    QCOMPARE(result.trustOf(i), FrameTrust::CascadedUntrusted);
    QVERIFY(!result.frames[i].cascadePath.empty());
  }
  QCOMPARE(result.trustOf(0), FrameTrust::Trusted);
  const QString reportPath = workdir + "/report.json";
  QVERIFY(DiagnosticReport::exportJson(result, reportPath).ok());
  const QString trajPath = workdir + "/trajectory.log";
  QFile traj(trajPath);
  traj.open(QIODevice::WriteOnly | QIODevice::Truncate);
  traj.write(DiagnosticReport::trajectoryLog(result));
  traj.close();
  QVERIFY(QFileInfo(trajPath).size() > 0);
}

void IncrementalSnapshotTest::snapshotRecoveryAndBrokenChain() {
  makeSamples(false);
  writeManifestV(1);
  ForensicEngine engine(workdir, 1700000000002ULL);
  RunStats stats;
  ForensicResult published;
  QVERIFY(engine.verify(seqPath, manifestPath, refPath, stats, published).ok());

  ForensicResult loaded;
  QByteArray hash;
  uint64_t version = 0;
  QVERIFY(engine.snapshots().loadLatest(loaded, hash, version).ok());
  QCOMPARE(loaded.frames.size(), published.frames.size());
  QCOMPARE(loaded.frames[3].contentHash, published.frames[3].contentHash);

  // Corrupt the snapshot body: hash chain verification must reject it.
  QFile pointer(engine.snapshots().dir() + "/latest.json");
  pointer.open(QIODevice::ReadOnly);
  const QString latestName = QString::fromUtf8(pointer.readAll()).trimmed();
  pointer.close();
  const QString snap = engine.snapshots().dir() + "/" + latestName;
  QFile f(snap);
  f.open(QIODevice::ReadOnly);
  QJsonObject env = QJsonDocument::fromJson(f.readAll()).object();
  f.close();
  // Tamper the recorded chain hash itself: recomputation over the body no
  // longer matches the envelope's recordHash -> E_RECOVERY_INVALID.
  QByteArray chain = QByteArray::fromHex(env.value("recordHash").toString().toLatin1());
  chain[0] = chain[0] ^ 0x01;
  env["recordHash"] = QString::fromLatin1(chain.toHex());
  f.open(QIODevice::WriteOnly | QIODevice::Truncate);
  f.write(QJsonDocument(env).toJson());
  f.close();

  ForensicResult broken;
  const Error re = engine.snapshots().loadLatest(broken, hash, version);
  QCOMPARE(re.code, StatusCode::E_RECOVERY_INVALID);
}

QTEST_GUILESS_MAIN(IncrementalSnapshotTest)
#include "test_incremental_snapshot.moc"
