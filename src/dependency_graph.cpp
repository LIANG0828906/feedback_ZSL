#include "vforensics/dependency_graph.h"
#include <algorithm>
#include <queue>
#include <unordered_set>

namespace vforensics {

Error DependencyGraph::validate(const std::vector<std::vector<uint32_t>>& references) {
  const uint32_t n = static_cast<uint32_t>(references.size());
  for (uint32_t i = 0; i < n; ++i) {
    for (uint32_t r : references[i]) {
      if (r >= n) {
        return Error::make(StatusCode::E_DEP_DANGLING,
                           QStringLiteral("frame %1 references missing frame %2").arg(i).arg(r));
      }
    }
  }
  // Kahn's algorithm on child->parent edges: detect any remaining cycle.
  std::vector<uint32_t> indegree(n, 0);
  for (uint32_t i = 0; i < n; ++i)
    for (uint32_t r : references[i])
      (void)r, ++indegree[i]; // each incoming reference contributes to child indegree
  std::queue<uint32_t> q;
  for (uint32_t i = 0; i < n; ++i) if (indegree[i] == 0) q.push(i);
  uint32_t visited = 0;
  while (!q.empty()) {
    const uint32_t cur = q.front(); q.pop();
    ++visited;
    for (uint32_t i = 0; i < n; ++i) {
      if (std::find(references[i].begin(), references[i].end(), cur) != references[i].end()) {
        if (--indegree[i] == 0) q.push(i);
      }
    }
  }
  if (visited != n) {
    return Error::make(StatusCode::E_DEP_CYCLE,
                       QStringLiteral("dependency cycle detected (%1 frames unresolved)").arg(n - visited));
  }
  return Error::success();
}

CascadeResult DependencyGraph::propagate(const std::vector<std::vector<uint32_t>>& references,
                                         const std::vector<uint32_t>& failureSources) {
  const uint32_t n = static_cast<uint32_t>(references.size());
  // Reverse adjacency: parent -> children that depend on it.
  std::vector<std::vector<uint32_t>> children(n);
  for (uint32_t child = 0; child < n; ++child) {
    for (uint32_t parent : references[child]) children[parent].push_back(child);
  }

  CascadeResult res;
  res.failureSources = failureSources;
  std::vector<int> parentOf(n, -1);
  std::unordered_set<uint32_t> seen;
  std::queue<uint32_t> q;
  for (uint32_t s : failureSources) {
    if (seen.insert(s).second) {
      q.push(s);
      parentOf[s] = -2; // marks a failure source
    }
  }
  while (!q.empty()) {
    const uint32_t cur = q.front(); q.pop();
    for (uint32_t child : children[cur]) {
      if (seen.insert(child).second) {
        parentOf[child] = static_cast<int>(cur);
        q.push(child);
      }
    }
  }
  res.affectedFrames.assign(seen.begin(), seen.end());
  std::sort(res.affectedFrames.begin(), res.affectedFrames.end());
  for (uint32_t f : res.affectedFrames) {
    std::vector<uint32_t> path;
    int cur = static_cast<int>(f);
    while (cur >= 0) {
      path.push_back(static_cast<uint32_t>(cur));
      cur = parentOf[cur] == -2 ? -1 : parentOf[cur];
    }
    std::reverse(path.begin(), path.end());
    res.propagationPaths.push_back(path);
  }
  std::unordered_set<QString> edgeSet;
  for (uint32_t child = 0; child < n; ++child) {
    if (!seen.count(child)) continue;
    for (uint32_t parent : references[child]) {
      if (seen.count(parent)) edgeSet.insert(QStringLiteral("%1->%2").arg(parent).arg(child));
    }
  }
  res.affectedEdges.assign(edgeSet.begin(), edgeSet.end());
  std::sort(res.affectedEdges.begin(), res.affectedEdges.end());
  return res;
}

std::vector<uint32_t> DependencyGraph::pathToFailure(
    const std::vector<std::vector<uint32_t>>& references,
    uint32_t frame,
    const std::vector<uint32_t>& failureSources) {
  const uint32_t n = static_cast<uint32_t>(references.size());
  if (frame >= n) return {};
  std::unordered_set<uint32_t> sources(failureSources.begin(), failureSources.end());
  std::vector<int> parentOf(n, -1);
  std::queue<uint32_t> q;
  q.push(frame);
  parentOf[frame] = -2;
  uint32_t hit = std::numeric_limits<uint32_t>::max();
  while (!q.empty() && hit == std::numeric_limits<uint32_t>::max()) {
    const uint32_t cur = q.front(); q.pop();
    if (sources.count(cur)) { hit = cur; break; }
    for (uint32_t p : references[cur]) {
      if (parentOf[p] == -1) { parentOf[p] = static_cast<int>(cur); q.push(p); }
    }
  }
  if (hit == std::numeric_limits<uint32_t>::max()) return {};
  std::vector<uint32_t> path;
  int cur = static_cast<int>(hit);
  while (cur != -2) {
    path.push_back(static_cast<uint32_t>(cur));
    cur = parentOf[cur];
  }
  std::reverse(path.begin(), path.end());
  return path;
}

} // namespace vforensics
