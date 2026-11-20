#include "vforensics/frame_sequence_writer.h"
#include "vforensics/byte_util.h"
#include "vforensics/frame_sequence.h"
#include <QSaveFile>

namespace vforensics {
namespace bu = byteutil;

Error FrameSequenceWriter::write(const QString& path, const SequenceHeader& header,
                                 const std::vector<QByteArray>& payloads,
                                 const std::vector<QByteArray>* checksumOverride) {
  if (payloads.size() != header.frameCount) {
    return Error::make(StatusCode::E_FRAME_INVALID, "writer payload count mismatch");
  }
  QByteArray out;
  out.append("VFSE", 4);
  bu::putU32(out, header.version);
  bu::putU32(out, header.width);
  bu::putU32(out, header.height);
  bu::putU32(out, header.channels);
  bu::putU32(out, header.frameCount);
  bu::putU64(out, header.baseTimestampUs);
  bu::putU64(out, header.framePeriodUs);

  for (uint32_t i = 0; i < header.frameCount; ++i) {
    const QByteArray& p = payloads[i];
    bu::putU32(out, i);
    bu::putU64(out, header.baseTimestampUs + uint64_t(i) * header.framePeriodUs);
    bu::putU32(out, static_cast<uint32_t>(p.size()));
    if (checksumOverride && i < checksumOverride->size() && !(*checksumOverride)[i].isEmpty()) {
      out.append((*checksumOverride)[i]);
    } else {
      out.append(FrameSequence::sha256(p));
    }
  }
  bu::putU32(out, 0x4C594150); // PAYL
  for (const QByteArray& p : payloads) out.append(p);

  QSaveFile sf(path);
  if (!sf.open(QIODevice::WriteOnly)) {
    return Error::make(StatusCode::E_IO, QStringLiteral("cannot write %1").arg(path));
  }
  sf.write(out);
  if (!sf.commit()) {
    return Error::make(StatusCode::E_IO, "frame sequence atomic write failed");
  }
  return Error::success();
}

} // namespace vforensics
