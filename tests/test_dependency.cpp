#include "vforensics/dependency_graph.h"
#include <algorithm>
#include <QtTest/QtTest>

using namespace vforensics;

class DependencyTest : public QObject {
  Q_OBJECT
private slots:
  void cascadeMarksEntireDownstream();
  void cycleRejected();
  void danglingRejected();
};

void DependencyTest::cascadeMarksEntireDownstream() {
  // 0 <- 1 <- 2 <- 3, plus 1 <- 4
  std::vector<std::vector<uint32_t>> refs(5);
  refs[1] = {0};
  refs[2] = {1};
  refs[3] = {2};
  refs[4] = {1};
  QVERIFY(DependencyGraph::validate(refs).ok());
  const CascadeResult r = DependencyGraph::propagate(refs, {1});
  QCOMPARE(r.affectedFrames.size(), 4u);
  QVERIFY(r.affectedFrames == std::vector<uint32_t>({1, 2, 3, 4}));
  const auto path3 = DependencyGraph::pathToFailure(refs, 3, {1});
  QVERIFY(path3 == std::vector<uint32_t>({3, 2, 1}));
  const auto hasEdge = [&](const QString& e) {
    return std::find(r.affectedEdges.begin(), r.affectedEdges.end(), e) != r.affectedEdges.end();
  };
  QVERIFY(hasEdge("1->2"));
  QVERIFY(hasEdge("2->3"));
  QVERIFY(hasEdge("1->4"));
}

void DependencyTest::cycleRejected() {
  std::vector<std::vector<uint32_t>> refs(3);
  refs[0] = {2};
  refs[1] = {0};
  refs[2] = {1};
  const Error e = DependencyGraph::validate(refs);
  QCOMPARE(e.code, StatusCode::E_DEP_CYCLE);
}

void DependencyTest::danglingRejected() {
  std::vector<std::vector<uint32_t>> refs(3);
  refs[2] = {9};
  const Error e = DependencyGraph::validate(refs);
  QCOMPARE(e.code, StatusCode::E_DEP_DANGLING);
}

QTEST_GUILESS_MAIN(DependencyTest)
#include "test_dependency.moc"
