#pragma once
#include <QByteArray>
#include <cstdint>

namespace vforensics::byteutil {

inline void putU32(QByteArray& b, uint32_t v) {
  b.append(static_cast<char>(v & 0xFF));
  b.append(static_cast<char>((v >> 8) & 0xFF));
  b.append(static_cast<char>((v >> 16) & 0xFF));
  b.append(static_cast<char>((v >> 24) & 0xFF));
}
inline void putU64(QByteArray& b, uint64_t v) {
  for (int i = 0; i < 8; ++i) b.append(static_cast<char>((v >> (8 * i)) & 0xFF));
}
inline uint32_t getU32(const QByteArray& b, qint64 off, bool& ok) {
  if (off < 0 || off + 4 > b.size()) { ok = false; return 0; }
  const auto* p = reinterpret_cast<const unsigned char*>(b.constData() + off);
  ok = true;
  return uint32_t(p[0]) | (uint32_t(p[1]) << 8) | (uint32_t(p[2]) << 16) | (uint32_t(p[3]) << 24);
}
inline uint64_t getU64(const QByteArray& b, qint64 off, bool& ok) {
  if (off < 0 || off + 8 > b.size()) { ok = false; return 0; }
  const auto* p = reinterpret_cast<const unsigned char*>(b.constData() + off);
  ok = true;
  uint64_t v = 0;
  for (int i = 0; i < 8; ++i) v |= uint64_t(p[i]) << (8 * i);
  return v;
}

} // namespace vforensics::byteutil
