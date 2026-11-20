#pragma once
#include <QString>
#include <QByteArray>
#include <QJsonObject>
#include <cstdint>
#include <vector>

namespace vforensics {

enum class StatusCode {
  Ok = 0,
  E_FRAME_INVALID,
  E_DEP_CYCLE,
  E_DEP_DANGLING,
  E_MANIFEST_VERSION,
  E_RECOVERY_INVALID,
  E_BUSY,
  E_REPORT_INVALID,
  E_IO,
  E_INTERNAL
};

QString statusName(StatusCode code);

struct Error {
  StatusCode code = StatusCode::Ok;
  QString message;
  bool ok() const { return code == StatusCode::Ok; }
  static Error success() { return {}; }
  static Error make(StatusCode c, const QString& msg) { return {c, msg}; }
};

// Frame record as declared by the binary sequence header.
struct FrameHeader {
  uint32_t index = 0;
  uint64_t timestampUs = 0;
  uint32_t dataLength = 0;
  QByteArray expectedHash; // raw 32-byte SHA-256 digest
};

struct SequenceHeader {
  uint32_t version = 0;
  uint32_t width = 0;
  uint32_t height = 0;
  uint32_t channels = 0;
  uint32_t frameCount = 0;
  uint64_t baseTimestampUs = 0;
  uint64_t framePeriodUs = 0;
};

struct FrameVerification {
  uint32_t index = 0;
  uint64_t timestampUs = 0;
  QByteArray contentHash;
  bool hashMatch = false;
  // Byte offset of the first mismatching pixel byte inside this frame.
  // -1 means mismatch position unavailable (e.g. checksum field missing).
  int64_t firstMismatchOffset = -1;
  // Absolute byte offset inside the whole .vfseq file payload section.
  int64_t firstMismatchAbsolute = -1;
  bool cascadeUntrusted = false;
  std::vector<uint32_t> cascadePath; // frame indices from a failed source to this frame
};

enum class FrameTrust {
  Trusted,
  HashMismatch,
  CascadedUntrusted
};

struct AnchorRecord {
  uint32_t segmentId = 0;
  uint32_t startFrame = 0;
  uint32_t endFrame = 0;
  uint32_t watermarkX = 0;
  uint32_t watermarkY = 0;
  QString referenceType;
};

struct AnchorDiff {
  uint32_t segmentId = 0;
  QString change; // ADDED / REMOVED / RANGE_CHANGED / WATERMARK_CHANGED / REFERENCE_TYPE_CHANGED
  QString detail;
};

struct ManifestData {
  uint32_t manifestVersion = 0;
  // Edge: frame index -> referenced parent frame indices.
  std::vector<std::vector<uint32_t>> references;
  std::vector<AnchorRecord> anchors;
  QByteArray rawHash;      // SHA-256 of the canonical manifest bytes
  QString sourcePath;
};

struct ForensicResult {
  SequenceHeader sequence;
  ManifestData manifest;
  std::vector<FrameVerification> frames;
  std::vector<AnchorDiff> anchorDiffs;
  uint64_t resultVersion = 0;
  uint64_t publishedAtMs = 0;
  QByteArray inputFileHash;

  bool frameFailed(uint32_t index) const;
  FrameTrust trustOf(uint32_t index) const;
  QJsonObject toJson() const;
};

} // namespace vforensics
