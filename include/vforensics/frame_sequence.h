#pragma once
#include "vforensics/types.h"
#include <QString>
#include <QByteArray>

namespace vforensics {

// Custom deterministic frame-sequence container (.vfseq).
//
// Layout (all integers little-endian):
//   magic[4]      "VFSE"
//   u32 formatVersion
//   u32 width, u32 height, u32 channels, u32 frameCount
//   u64 baseTimestampUs, u64 framePeriodUs
//   per frame, frameCount times:
//       u32 index, u64 timestampUs, u32 dataLength, u8 sha256[32]
//   u32 payloadMagic (0x5041594C "LAYL")
//   per frame, frameCount times, dataLength bytes of raw pixels
//
// Structural contradictions (bad magic, truncated stream, declared data
// length != remaining bytes, missing/zero checksum) fail the ENTIRE parse
// with E_FRAME_INVALID: no partially parsed frame is ever trusted.
class FrameSequence {
public:
  static Error parse(const QString& path, FrameSequence& out);

  const SequenceHeader& header() const { return header_; }
  const std::vector<FrameHeader>& frameHeaders() const { return frameHeaders_; }
  const QByteArray& bytes() const { return data_; }
  QByteArray framePayload(const FrameHeader& fh) const;
  int64_t payloadAbsoluteOffset(uint32_t frameIndex) const;

  static QByteArray sha256(const QByteArray& bytes);
  // First differing byte, or -1 when lengths/bytes are identical.
  static int64_t firstDiffOffset(const QByteArray& a, const QByteArray& b);

private:
  SequenceHeader header_{};
  std::vector<FrameHeader> frameHeaders_;
  QByteArray data_;
  std::vector<int64_t> payloadOffsets_;
};

} // namespace vforensics
