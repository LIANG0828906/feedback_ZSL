#pragma once

#include "core/Error.h"
#include "core/Mesh.h"
#include "core/Welding.h"

#include <optional>
#include <string>
#include <vector>

namespace meshedit {

struct Selection {
    std::vector<int> vertices;
    std::vector<int> faces;

    bool empty() const { return vertices.empty() && faces.empty(); }
};

struct TransformSpec {
    Vec3 translation{};
    Vec3 rotationAxis{0.0, 0.0, 1.0};
    double rotationDegrees = 0.0;
    Vec3 scale{1.0, 1.0, 1.0};
    Vec3 pivot{};
};

struct EditOutcome {
    Mesh mesh;
    MeshMetrics beforeMetrics;
    MeshMetrics afterMetrics;
    std::string operation;
    std::string detail;
    std::size_t affectedFaceCount = 0;
    std::optional<WeldReport> weldReport;
};

struct EditRecord {
    std::string operation;
    std::string detail;
    Mesh before;
    Mesh after;
    MeshMetrics beforeMetrics;
    MeshMetrics afterMetrics;
};

class EditHistory {
public:
    void clear();
    void push(EditRecord record);
    bool canUndo() const { return !undo_.empty(); }
    bool canRedo() const { return !redo_.empty(); }
    std::size_t undoCount() const { return undo_.size(); }
    std::size_t redoCount() const { return redo_.size(); }

    bool undo(Mesh& current);
    bool redo(Mesh& current);

    const std::vector<EditRecord>& undoRecords() const { return undo_; }
    const std::vector<EditRecord>& redoRecords() const { return redo_; }
    std::vector<EditRecord>& undoRecords() { return undo_; }
    std::vector<EditRecord>& redoRecords() { return redo_; }

private:
    std::vector<EditRecord> undo_;
    std::vector<EditRecord> redo_;
};

struct SimplifyOptions {
    double targetRatio = 0.5;
};

Selection selectionFromFaces(const Mesh& mesh, const std::vector<int>& faceIds);
Selection selectVerticesInBounds(const Mesh& mesh, const Aabb& bounds);
Selection selectFacesInBounds(const Mesh& mesh, const Aabb& bounds);

bool transformSelection(Mesh& mesh, const Selection& selection,
                        const TransformSpec& transform, std::string& error);

Result<EditOutcome> applyTransform(const Mesh& input, const Selection& selection,
                                   const TransformSpec& transform);
Result<EditOutcome> applySimplify(const Mesh& input, const SimplifyOptions& options);

struct MeshDocument {
    Mesh current;
    Mesh baseline;
    EditHistory history;
    int exportVersion = 2;
    double weldTolerance = 1e-6;

    void load(Mesh mesh);
    void commit(const EditOutcome& outcome);
    bool undo();
    bool redo();
    MeshMetrics metrics() const { return computeMetrics(current); }
};

std::string editRecordJson(const EditRecord& record);

} // namespace meshedit
