#pragma once
#include "vforensics/types.h"
#include <QString>
#include <QByteArray>
#include <vector>

namespace vforensics {

class FrameSequenceWriter {
public:
  // Writes a .vfseq container. When checksumsOverride[i] is non-null it
  // replaces the honest SHA-256 of frame i (used to simulate local
  // re-encoding / corrupted checksum fields).
  static Error write(const QString& path,
                     const SequenceHeader& header,
                     const std::vector<QByteArray>& payloads,
                     const std::vector<QByteArray>* checksumOverride = nullptr);
};

} // namespace vforensics
