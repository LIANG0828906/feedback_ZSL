#include "vforensics/frame_sequence.h"
#include "vforensics/frame_sequence_writer.h"
#include <QCommandLineParser>
#include <QDir>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QSaveFile>
#include <QTextStream>
#include <opencv2/core.hpp>
#include <cstdint>

using namespace vforensics;

namespace {

// Deterministic LCG: identical seed -> identical pixel bytes, no entropy
// sources, no clock input, so generated samples are bit-for-bit reproducible.
class Lcg {
public:
  explicit Lcg(uint64_t seed) : state_(seed ? seed : 1) {}
  uint32_t next() {
    state_ = state_ * 6364136223846793005ULL + 1442695040888963407ULL;
    return static_cast<uint32_t>(state_ >> 32);
  }
private:
  uint64_t state_;
};

QJsonArray uintArray(const std::vector<uint32_t>& v) {
  QJsonArray a;
  for (uint32_t x : v) a.append(static_cast<int>(x));
  return a;
}

void writeManifest(const QString& path, uint32_t version, uint32_t frameCount,
                   const std::vector<std::vector<uint32_t>>& refs,
                   const std::vector<AnchorRecord>& anchors) {
  QJsonObject root;
  root["manifestVersion"] = static_cast<int>(version);
  QJsonArray edges;
  for (uint32_t i = 0; i < frameCount; ++i) {
    QJsonObject e;
    e["frame"] = static_cast<int>(i);
    e["references"] = uintArray(refs[i]);
    edges.append(e);
  }
  root["dependencies"] = edges;
  QJsonArray ja;
  for (const AnchorRecord& a : anchors) {
    QJsonObject o;
    o["segmentId"] = static_cast<int>(a.segmentId);
    o["startFrame"] = static_cast<int>(a.startFrame);
    o["endFrame"] = static_cast<int>(a.endFrame);
    o["watermarkX"] = static_cast<int>(a.watermarkX);
    o["watermarkY"] = static_cast<int>(a.watermarkY);
    o["referenceType"] = a.referenceType;
    ja.append(o);
  }
  root["anchors"] = ja;
  QSaveFile sf(path);
  sf.open(QIODevice::WriteOnly);
  sf.write(QJsonDocument(root).toJson(QJsonDocument::Indented));
  sf.commit();
}

} // namespace

int main(int argc, char** argv) {
  QCoreApplication app(argc, argv);
  QCommandLineParser parser;
  parser.setApplicationDescription("Deterministic forensic sample generator");
  QCommandLineOption configOpt({"c", "config"}, "Profile config JSON", "config");
  QCommandLineOption outOpt({"o", "out"}, "Output directory", "out");
  parser.addOption(configOpt);
  parser.addOption(outOpt);
  parser.addHelpOption();
  parser.process(app);

  QFile cf(parser.value(configOpt));
  if (!cf.open(QIODevice::ReadOnly)) {
    QTextStream(stderr) << "cannot open config\n";
    return 1;
  }
  const QJsonObject cfg = QJsonDocument::fromJson(cf.readAll()).object();
  const uint64_t seed = cfg.value("seed").toVariant().toULongLong();
  const QString outDir = parser.value(outOpt);
  QDir().mkpath(outDir);

  for (const QJsonValue& pv : cfg.value("profiles").toArray()) {
    const QJsonObject p = pv.toObject();
    const QString name = p.value("name").toString();
    SequenceHeader h;
    h.version = 1;
    h.width = p.value("width").toInt();
    h.height = p.value("height").toInt();
    h.channels = p.value("channels").toInt();
    h.frameCount = p.value("frameCount").toInt();
    h.baseTimestampUs = 1000000;
    h.framePeriodUs = 40000;

    // Dependency chain: frame 0 is a key frame; every later P-frame
    // references the immediately previous frame.
    std::vector<std::vector<uint32_t>> refs(h.frameCount);
    for (uint32_t i = 1; i < h.frameCount; ++i) refs[i].push_back(i - 1);

    std::vector<AnchorRecord> anchors;
    const uint32_t manifestVersion = p.value("manifestVersion").toInt();
    if (manifestVersion == 1) {
      anchors.push_back({0, 0, h.frameCount / 2 - 1, 2, 2, "KEYFRAME"});
      anchors.push_back({1, h.frameCount / 2, h.frameCount - 1, 4, 4, "PREDICTED"});
    } else {
      // v2 changes anchor ranges, watermark position and reference type so
      // the engine's semantic diffing has observable changes to report.
      anchors.push_back({0, 0, h.frameCount / 2, 2, 3, "KEYFRAME_WATERMARKED"});
      anchors.push_back({1, h.frameCount / 2 + 1, h.frameCount - 1, 6, 5, "PREDICTED_CHAIN"});
    }

    Lcg lcg(seed ^ (uint64_t(qHash(name)) << 32) ^ h.frameCount);
    std::vector<QByteArray> original;
    std::vector<QByteArray> tampered;
    original.reserve(h.frameCount);
    tampered.reserve(h.frameCount);
    for (uint32_t f = 0; f < h.frameCount; ++f) {
      cv::Mat img(h.height, h.width, CV_8UC(h.channels));
      for (int y = 0; y < h.height; ++y) {
        for (int x = 0; x < h.width; ++x) {
          unsigned char* px = img.ptr<unsigned char>(y, x);
          for (int c = 0; c < h.channels; ++c) {
            px[c] = static_cast<unsigned char>(lcg.next() ^ (f * 37 + x * 7 + y * 11 + c * 5));
          }
        }
      }
      const QByteArray raw(reinterpret_cast<const char*>(img.data),
                           static_cast<int>(img.total() * img.elemSize()));
      original.push_back(raw);
      tampered.push_back(raw);
    }

    QSet<int> tamperSet;
    for (const QJsonValue& tv : p.value("tamperFrames").toArray()) tamperSet.insert(tv.toInt());
    const int tamperOffset = p.value("tamperByteOffset").toInt(0);
    for (int frameIdx : tamperSet) {
      // Simulate local re-encoding: flip one pixel byte while the header
      // still carries the checksum of the original pixels.
      tampered[frameIdx][tamperOffset] =
          static_cast<char>(static_cast<unsigned char>(tampered[frameIdx][tamperOffset]) ^ 0xFF);
    }

    const QString base = QDir(outDir).filePath(name);
    // The tampered .vfseq keeps the original (stale) per-frame checksums.
    std::vector<QByteArray> originalChecksums;
    for (const QByteArray& p : original) originalChecksums.push_back(FrameSequence::sha256(p));
    Error e1 = FrameSequenceWriter::write(base + ".vfseq", h, tampered, &originalChecksums);
    Error e2 = FrameSequenceWriter::write(base + ".vref", h, original);
    writeManifest(base + ".manifest.json", manifestVersion, h.frameCount, refs, anchors);
    if (!e1.ok() || !e2.ok()) {
      QTextStream(stderr) << "sample write failed for " << name << "\n";
      return 2;
    }
    QTextStream(stdout) << "generated " << name << " frames=" << h.frameCount
                        << " tampered=" << tamperSet.size() << " manifest=v"
                        << manifestVersion << "\n";
  }
  return 0;
}
