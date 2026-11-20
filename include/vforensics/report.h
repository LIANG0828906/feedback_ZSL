#pragma once
#include "vforensics/types.h"
#include <QString>

namespace vforensics {

class DiagnosticReport {
public:
  // Exports a JSON report. Fails with E_REPORT_INVALID if any required
  // evidence field (frame number, byte offset, cascade path, manifest version)
  // is missing or internally inconsistent.
  static Error exportJson(const ForensicResult& result, const QString& outPath);

  // Deterministic trajectory log line set used by the demo harness.
  static QByteArray trajectoryLog(const ForensicResult& result);
};

} // namespace vforensics
