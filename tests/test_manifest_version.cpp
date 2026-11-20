#include "vforensics/manifest.h"
#include <QDir>
#include <QFile>
#include <QJsonDocument>
#include <QtTest/QtTest>

using namespace vforensics;

class ManifestVersionTest : public QObject {
  Q_OBJECT
  QString writeManifest(int version, bool withAnchors, bool changeSemantics);
private slots:
  void upgradeProducesAnchorDiff();
  void implicitMigrationRejected();
};

QString ManifestVersionTest::writeManifest(int version, bool withAnchors, bool change) {
  QJsonObject root;
  root["manifestVersion"] = version;
  QJsonArray deps;
  for (int i = 0; i < 6; ++i) {
    QJsonObject e;
    e["frame"] = i;
    QJsonArray refs;
    if (i > 0) refs.append(i - 1);
    e["references"] = refs;
    deps.append(e);
  }
  root["dependencies"] = deps;
  QJsonArray anchors;
  if (withAnchors) {
    QJsonObject a;
    a["segmentId"] = 0;
    a["startFrame"] = 0;
    a["endFrame"] = change ? 3 : 2;
    a["watermarkX"] = change ? 9 : 2;
    a["watermarkY"] = 2;
    a["referenceType"] = change ? "KEYFRAME_WATERMARKED" : "KEYFRAME";
    anchors.append(a);
  }
  root["anchors"] = anchors;
  const QString path = QDir::tempPath() + QStringLiteral("/vf_manifest_%1.json").arg(version);
  QFile f(path);
  f.open(QIODevice::WriteOnly | QIODevice::Truncate);
  f.write(QJsonDocument(root).toJson());
  f.close();
  return path;
}

void ManifestVersionTest::upgradeProducesAnchorDiff() {
  const QString v1 = writeManifest(1, true, false);
  const QString v2 = writeManifest(2, true, true);
  ManifestData oldM;
  QVERIFY(Manifest::parse(v1, 6, oldM).ok());
  ManifestData newM;
  std::vector<AnchorDiff> diffs;
  const Error e = Manifest::parse(v2, 6, newM, &diffs, &oldM);
  QVERIFY2(e.ok(), e.message.toLocal8Bit());
  QVERIFY(!diffs.empty());
  const QString changes = [&] {
    QString s;
    for (const auto& d : diffs) s += d.change + " ";
    return s;
  }();
  QVERIFY(changes.contains("RANGE_CHANGED"));
  QVERIFY(changes.contains("WATERMARK_CHANGED"));
  QVERIFY(changes.contains("REFERENCE_TYPE_CHANGED"));
}

void ManifestVersionTest::implicitMigrationRejected() {
  const QString v1 = writeManifest(1, true, false);
  const QString v2 = writeManifest(2, false, false);
  ManifestData oldM;
  QVERIFY(Manifest::parse(v1, 6, oldM).ok());
  ManifestData newM;
  const Error e = Manifest::parse(v2, 6, newM, nullptr, &oldM);
  QCOMPARE(e.code, StatusCode::E_MANIFEST_VERSION);
}

QTEST_GUILESS_MAIN(ManifestVersionTest)
#include "test_manifest_version.moc"
