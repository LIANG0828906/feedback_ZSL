#include "vforensics/frame_sequence.h"
#include "vforensics/byte_util.h"
#include <QCryptographicHash>
#include <QFile>
#include <cstring>

namespace vforensics {
namespace bu = byteutil;

namespace {
constexpr char kMagic[4] = {'V', 'F', 'S', 'E'};
constexpr uint32_t kPayloadMagic = 0x4C594150; // "PAYL" little-endian
constexpr uint32_t kSupportedVersion = 1;
}

QByteArray FrameSequence::sha256(const QByteArray& bytes) {
  return QCryptographicHash::hash(bytes, QCryptographicHash::Sha256);
}

int64_t FrameSequence::firstDiffOffset(const QByteArray& a, const QByteArray& b) {
  const qint64 n = std::min(a.size(), b.size());
  for (qint64 i = 0; i < n; ++i) {
    if (a[i] != b[i]) return i;
  }
  return a.size() == b.size() ? -1 : n;
}

Error FrameSequence::parse(const QString& path, FrameSequence& out) {
  QFile f(path);
  if (!f.open(QIODevice::ReadOnly)) {
    return Error::make(StatusCode::E_IO, QStringLiteral("cannot open frame sequence: %1").arg(path));
  }
  QByteArray bytes = f.readAll();
  f.close();

  const auto fail = [](const QString& m) {
    // Requirement 1: structural contradiction invalidates the whole file,
    // callers must not partially trust any frame already decoded.
    return Error::make(StatusCode::E_FRAME_INVALID, m);
  };

  if (bytes.size() < 4 || std::memcmp(bytes.constData(), kMagic, 4) != 0) {
    return fail("bad magic, not a VFSE frame sequence");
  }
  bool ok = true;
  qint64 pos = 4;
  SequenceHeader h;
  h.version = bu::getU32(bytes, pos, ok); pos += 4;
  if (!ok || h.version != kSupportedVersion) return fail("unsupported format version");
  h.width = bu::getU32(bytes, pos, ok); pos += 4;
  h.height = bu::getU32(bytes, pos, ok); pos += 4;
  h.channels = bu::getU32(bytes, pos, ok); pos += 4;
  h.frameCount = bu::getU32(bytes, pos, ok); pos += 4;
  h.baseTimestampUs = bu::getU64(bytes, pos, ok); pos += 8;
  h.framePeriodUs = bu::getU64(bytes, pos, ok); pos += 8;
  if (!ok) return fail("truncated fixed header");
  if (h.width == 0 || h.height == 0 || h.channels == 0 || h.channels > 4) {
    return fail("invalid resolution / channel count");
  }
  if (h.frameCount == 0) return fail("frame count must be non-zero");

  std::vector<FrameHeader> fhs;
  fhs.reserve(h.frameCount);
  for (uint32_t i = 0; i < h.frameCount; ++i) {
    FrameHeader fh;
    fh.index = bu::getU32(bytes, pos, ok); pos += 4;
    fh.timestampUs = bu::getU64(bytes, pos, ok); pos += 8;
    fh.dataLength = bu::getU32(bytes, pos, ok); pos += 4;
    if (!ok) return fail("truncated per-frame header");
    if (pos + 32 > bytes.size()) return fail("truncated checksum field");
    fh.expectedHash = QByteArray(bytes.constData() + pos, 32);
    pos += 32;
    if (fh.index != i) return fail(QStringLiteral("frame index out of order at slot %1").arg(i));
    if (fh.dataLength == 0) return fail(QStringLiteral("zero-length frame payload at %1").arg(i));
    // Missing checksum is explicitly invalid rather than silently skipped.
    bool nonZeroChecksum = false;
    for (char c : fh.expectedHash) if (c != 0) { nonZeroChecksum = true; break; }
    if (!nonZeroChecksum) return fail(QStringLiteral("missing checksum field at frame %1").arg(i));
    fhs.push_back(fh);
  }

  const uint32_t payloadMagic = bu::getU32(bytes, pos, ok); pos += 4;
  if (!ok || payloadMagic != kPayloadMagic) return fail("payload section magic missing");

  std::vector<int64_t> offsets;
  offsets.reserve(h.frameCount);
  for (uint32_t i = 0; i < h.frameCount; ++i) {
    offsets.push_back(pos);
    pos += fhs[i].dataLength;
    if (pos > bytes.size()) return fail(QStringLiteral("payload truncated at frame %1").arg(i));
  }
  if (pos != bytes.size()) {
    return fail(QStringLiteral("declared data length contradicts file size (%1 != %2)").arg(pos).arg(bytes.size()));
  }

  out.header_ = h;
  out.frameHeaders_ = std::move(fhs);
  out.data_ = std::move(bytes);
  out.payloadOffsets_ = std::move(offsets);
  return Error::success();
}

QByteArray FrameSequence::framePayload(const FrameHeader& fh) const {
  const auto it = std::find_if(frameHeaders_.begin(), frameHeaders_.end(),
                               [&](const FrameHeader& x) { return x.index == fh.index; });
  if (it == frameHeaders_.end()) return {};
  const size_t slot = static_cast<size_t>(std::distance(frameHeaders_.begin(), it));
  return QByteArray(data_.constData() + payloadOffsets_[slot], fh.dataLength);
}

int64_t FrameSequence::payloadAbsoluteOffset(uint32_t frameIndex) const {
  if (frameIndex >= payloadOffsets_.size()) return -1;
  return payloadOffsets_[frameIndex];
}

} // namespace vforensics
