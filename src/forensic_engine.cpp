#include "vforensics/forensic_engine.h"
#include "vforensics/frame_sequence.h"
#include "vforensics/manifest.h"
#include "vforensics/dependency_graph.h"
#include <QDir>
#include <QFile>
#include <QDateTime>

namespace vforensics {

ForensicEngine::ForensicEngine(QString workdir, uint64_t fixedClockMs)
    : workdir_(std::move(workdir)),
      fixedClockMs_(fixedClockMs),
      cache_(QDir(workdir_).filePath("cache")),
      snapshotStore_(workdir_) {}

namespace {

// Reference sidecar (.vref): same container format as .vfseq, holding the
// original (un-tampered) pixels. It is optional and only used to localize the
// first mismatching byte with byte-level precision.
Error loadReference(const QString& path, std::vector<QByteArray>& payloads) {
  payloads.clear();
  if (path.isEmpty()) return Error::success();
  FrameSequence ref;
  Error e = FrameSequence::parse(path, ref);
  if (!e.ok()) return e;
  for (const FrameHeader& fh : ref.frameHeaders()) payloads.push_back(ref.framePayload(fh));
  return Error::success();
}

} // namespace

Error ForensicEngine::runInternal(const QString& sequencePath, const QString& manifestPath,
                                  const QString& referencePath, bool resumeMode,
                                  RunStats& stats, ForensicResult& resultOut,
                                  const ProgressCb& progress,
                                  const std::atomic<bool>* cancelToken) {
  stats = {};
  Error e = cache_.load();
  if (!e.ok()) return e;

  FrameSequence seq;
  e = FrameSequence::parse(sequencePath, seq);
  if (!e.ok()) return e;

  // Previous manifest (if any) enables version-upgrade anchor diffing.
  std::optional<ManifestData> previousManifest;
  ForensicResult previousResult;
  QByteArray prevRecordHash(32, '\0');
  if (resumeMode) {
    uint64_t prevVersion = 0;
    Error re = snapshotStore_.loadLatest(previousResult, prevRecordHash, prevVersion);
    if (!re.ok()) return re; // E_RECOVERY_INVALID: do not overwrite old snapshot
    previousManifest = previousResult.manifest;
  } else {
    QByteArray dummyHash;
    uint64_t prevVersion = 0;
    Error re = snapshotStore_.loadLatest(previousResult, dummyHash, prevVersion);
    if (re.ok()) {
      prevRecordHash = dummyHash;
      previousManifest = previousResult.manifest;
    }
  }

  ManifestData manifest;
  std::vector<AnchorDiff> anchorDiffs;
  e = Manifest::parse(manifestPath, seq.header().frameCount, manifest,
                      previousManifest ? &anchorDiffs : nullptr,
                      previousManifest ? &*previousManifest : nullptr);
  if (!e.ok()) return e;

  e = DependencyGraph::validate(manifest.references);
  if (!e.ok()) return e; // E_DEP_CYCLE / E_DEP_DANGLING: no trust conclusion emitted

  std::vector<QByteArray> referencePayloads;
  e = loadReference(referencePath, referencePayloads);
  if (!e.ok()) return e;

  QFile sf(sequencePath);
  if (!sf.open(QIODevice::ReadOnly)) return Error::make(StatusCode::E_IO, "cannot reopen sequence");
  const QByteArray fileBytes = sf.readAll();
  sf.close();
  const QByteArray inputHash = FrameSequence::sha256(fileBytes);

  ForensicResult result;
  result.sequence = seq.header();
  result.manifest = manifest;
  result.anchorDiffs = anchorDiffs;
  result.inputFileHash = inputHash;
  result.frames.resize(seq.header().frameCount);

  // Resume: keep previously verified frames; hash chain validation above
  // already proved they were not tampered with inside the snapshot.
  std::vector<bool> done(seq.header().frameCount, false);
  if (resumeMode && previousResult.manifest.rawHash == manifest.rawHash &&
      previousResult.inputFileHash == inputHash &&
      previousResult.frames.size() == result.frames.size()) {
    for (const FrameVerification& fv : previousResult.frames) {
      if (!fv.contentHash.isEmpty()) {
        result.frames[fv.index] = fv;
        done[fv.index] = true;
        ++stats.reusedFrames;
      }
    }
  }

  const auto& fhs = seq.frameHeaders();
  for (uint32_t i = 0; i < seq.header().frameCount; ++i) {
    if (done[i]) {
      if (progress) progress(i + 1, seq.header().frameCount);
      continue;
    }
    const FrameHeader& fh = fhs[i];
    const QByteArray payload = seq.framePayload(fh);
    const QByteArray contentHash = FrameSequence::sha256(payload);
    const bool match = (contentHash == fh.expectedHash);

    // Incremental reuse: cache hit only when the declared checksum AND the
    // recomputed content hash both agree with the stored record.
    const QString fkey = CacheStore::frameKey(fh.expectedHash, contentHash, i);
    if (const CachedFrame* cached = cache_.lookupFrame(fkey)) {
      if (cached->contentHash == contentHash) {
        ++stats.reusedFrames;
      } else {
        ++stats.recomputedFrames;
      }
    } else {
      ++stats.recomputedFrames;
    }
    CachedFrame cf{fh.expectedHash, contentHash, match};
    cache_.putFrame(fkey, cf);

    FrameVerification fv;
    fv.index = i;
    fv.timestampUs = fh.timestampUs;
    fv.contentHash = contentHash;
    fv.hashMatch = match;
    if (!match && i < referencePayloads.size()) {
      const QByteArray& original = referencePayloads[i];
      const int64_t local = FrameSequence::firstDiffOffset(payload, original);
      fv.firstMismatchOffset = local;
      const int64_t absBase = seq.payloadAbsoluteOffset(i);
      fv.firstMismatchAbsolute = local >= 0 ? absBase + local : absBase;
    }
    result.frames[i] = fv;
    if (cancelToken && cancelToken->load()) {
      // Cancellation: drop task-owned handles by going out of scope; the
      // previously published snapshot stays untouched and readable.
      return Error::make(StatusCode::E_BUSY, "verification cancelled before publish");
    }
    if (progress) progress(i + 1, seq.header().frameCount);
  }

  // Requirement 2: failed frames are failure sources; everything reachable
  // downstream along dependency edges is marked untrusted.
  std::vector<uint32_t> sources;
  for (const FrameVerification& fv : result.frames) {
    if (!fv.hashMatch) sources.push_back(fv.index);
  }
  const CascadeResult cascade = DependencyGraph::propagate(manifest.references, sources);
  stats.affectedFrames = static_cast<uint32_t>(cascade.affectedFrames.size());

  for (uint32_t f : cascade.affectedFrames) {
    const auto pathIt = std::find_if(cascade.propagationPaths.begin(), cascade.propagationPaths.end(),
                                     [&](const std::vector<uint32_t>& p) { return !p.empty() && p.back() == f; });
    std::vector<uint32_t> path = pathIt != cascade.propagationPaths.end() ? *pathIt
                                                                          : std::vector<uint32_t>{f};
    const QString ckey = CacheStore::cascadeKey(manifest.rawHash, f);
    CachedCascade cc;
    cc.cascadeUntrusted = true;
    cc.cascadePath = path;
    cache_.putCascade(ckey, cc);
    result.frames[f].cascadeUntrusted = true;
    result.frames[f].cascadePath = path;
  }

  // Monotonic result version; deterministic when a fixed clock is supplied.
  result.resultVersion = resumeMode ? previousResult.resultVersion + 1
                                    : (previousResult.resultVersion ? previousResult.resultVersion + 1 : 1);
  result.publishedAtMs = fixedClockMs_ ? fixedClockMs_
                                       : QDateTime::currentMSecsSinceEpoch();

  QByteArray newHash;
  e = snapshotStore_.publish(result, prevRecordHash, newHash, stats.publishedSnapshot);
  if (!e.ok()) return e;
  e = cache_.save();
  if (!e.ok()) return e;

  stats.resultVersion = result.resultVersion;
  resultOut = std::move(result);
  return Error::success();
}

Error ForensicEngine::verify(const QString& sequencePath, const QString& manifestPath,
                             const QString& referencePath, RunStats& stats,
                             ForensicResult& result, const ProgressCb& progress,
                             const std::atomic<bool>* cancelToken) {
  return runInternal(sequencePath, manifestPath, referencePath, false, stats, result, progress, cancelToken);
}

Error ForensicEngine::resume(const QString& sequencePath, const QString& manifestPath,
                             const QString& referencePath, RunStats& stats,
                             ForensicResult& result) {
  return runInternal(sequencePath, manifestPath, referencePath, true, stats, result, {}, nullptr);
}

} // namespace vforensics
