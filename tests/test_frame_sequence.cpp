#include "vforensics/frame_sequence.h"
#include "vforensics/frame_sequence_writer.h"
#include <QtTest/QtTest>
#include <QDir>
#include <QFile>

using namespace vforensics;

class FrameSequenceTest : public QObject {
  Q_OBJECT
private slots:
  void parseAndHashMatch();
  void tamperedFrameReportsFirstOffset();
  void truncatedFileFailsWholeParse();
  void missingChecksumRejected();
};

namespace {
SequenceHeader makeHeader(uint32_t frames = 4) {
  return {1, 4, 4, 3, frames, 1000, 40000};
}
}

void FrameSequenceTest::parseAndHashMatch() {
  SequenceHeader h = makeHeader();
  std::vector<QByteArray> payloads;
  for (uint32_t i = 0; i < h.frameCount; ++i) {
    payloads.emplace_back(QByteArray(4 * 4 * 3, static_cast<char>(0x10 + i)));
  }
  const QString path = QDir::tempPath() + "/vf_fs_ok.vfseq";
  QVERIFY(FrameSequenceWriter::write(path, h, payloads).ok());
  FrameSequence seq;
  QVERIFY(FrameSequence::parse(path, seq).ok());
  QCOMPARE(seq.header().frameCount, 4u);
  for (uint32_t i = 0; i < h.frameCount; ++i) {
    const QByteArray digest = FrameSequence::sha256(seq.framePayload(seq.frameHeaders()[i]));
    QCOMPARE(digest, seq.frameHeaders()[i].expectedHash);
  }
}

void FrameSequenceTest::tamperedFrameReportsFirstOffset() {
  SequenceHeader h = makeHeader();
  std::vector<QByteArray> original;
  for (uint32_t i = 0; i < h.frameCount; ++i)
    original.emplace_back(QByteArray(48, static_cast<char>(i + 1)));
  std::vector<QByteArray> tampered = original;
  tampered[2][7] = char(0xAB);
  const QString seqPath = QDir::tempPath() + "/vf_fs_tamper.vfseq";
  const QString refPath = QDir::tempPath() + "/vf_fs_tamper.vref";
  // Header keeps the ORIGINAL checksums while payloads carry the tampered
  // bytes, reproducing local re-encoding with stale checksum fields.
  std::vector<QByteArray> honestChecksums;
  for (const QByteArray& p : original) honestChecksums.push_back(FrameSequence::sha256(p));
  QVERIFY(FrameSequenceWriter::write(seqPath, h, tampered, &honestChecksums).ok());
  QVERIFY(FrameSequenceWriter::write(refPath, h, original).ok());
  FrameSequence seq, ref;
  QVERIFY(FrameSequence::parse(seqPath, seq).ok());
  QVERIFY(FrameSequence::parse(refPath, ref).ok());
  QVERIFY(FrameSequence::sha256(seq.framePayload(seq.frameHeaders()[2])) !=
          seq.frameHeaders()[2].expectedHash);
  const int64_t off = FrameSequence::firstDiffOffset(
      seq.framePayload(seq.frameHeaders()[2]), ref.framePayload(ref.frameHeaders()[2]));
  QCOMPARE(off, 7);
}

void FrameSequenceTest::truncatedFileFailsWholeParse() {
  SequenceHeader h = makeHeader();
  std::vector<QByteArray> payloads(h.frameCount, QByteArray(48, 'x'));
  const QString path = QDir::tempPath() + "/vf_fs_trunc.vfseq";
  QVERIFY(FrameSequenceWriter::write(path, h, payloads).ok());
  QFile f(path);
  f.open(QIODevice::ReadWrite);
  f.resize(f.size() - 9); // chop payload bytes -> length contradiction
  f.close();
  FrameSequence seq;
  const Error e = FrameSequence::parse(path, seq);
  QCOMPARE(e.code, StatusCode::E_FRAME_INVALID);
}

void FrameSequenceTest::missingChecksumRejected() {
  SequenceHeader h = makeHeader();
  std::vector<QByteArray> payloads(h.frameCount, QByteArray(48, 'z'));
  std::vector<QByteArray> overrides(h.frameCount);
  overrides[1] = QByteArray(32, '\0'); // zeroed checksum = missing
  const QString path = QDir::tempPath() + "/vf_fs_nocheck.vfseq";
  QVERIFY(FrameSequenceWriter::write(path, h, payloads, &overrides).ok());
  FrameSequence seq;
  const Error e = FrameSequence::parse(path, seq);
  QCOMPARE(e.code, StatusCode::E_FRAME_INVALID);
}

QTEST_GUILESS_MAIN(FrameSequenceTest)
#include "test_frame_sequence.moc"
