using System.Numerics;
using System.Security.Cryptography;

namespace GameEditor.Spatial;

public sealed class GeneratedSample
{
    public required SceneDocument Scene { get; init; }
    public required QueryManifest Queries { get; init; }
    public required GroundTruthManifest GroundTruth { get; init; }
    public required EditSequenceDefinition Edits { get; init; }
    public required EditTruthManifest EditTruth { get; init; }
}

public static class SampleFiles
{
    public const string Scene = "scene.json";
    public const string Queries = "queries.json";
    public const string GroundTruth = "ground-truth.json";
    public const string Edits = "edits.json";
    public const string EditTruth = "edit-truth.json";
    public const string Manifest = "manifest.json";
}

public static class DeterministicSceneGenerator
{
    public const int DefaultSeed = 424242;

    public static GeneratedSample Generate(int seed = DefaultSeed)
    {
        var scene = BuildScene(seed);
        var queries = BuildQueries();
        queries.SceneContentHash = scene.Declaration.ContentHash;
        var state = SceneState.FromDocument(scene);
        var groundTruth = new GroundTruthManifest
        {
            SceneContentHash = scene.Declaration.ContentHash,
            Rays = ReferenceEvaluator.BuildRayTruth(state, queries),
            Boxes = ReferenceEvaluator.BuildBoxTruth(state, queries)
        };
        var edits = BuildEdits(scene.Declaration.ContentHash);
        var editTruth = BuildEditTruth(state, queries, edits);
        return new GeneratedSample
        {
            Scene = scene,
            Queries = queries,
            GroundTruth = groundTruth,
            Edits = edits,
            EditTruth = editTruth
        };
    }

    public static Result<bool> Write(GeneratedSample sample, string directory)
    {
        try
        {
            Directory.CreateDirectory(directory);
        }
        catch (Exception exception) when (exception is IOException or UnauthorizedAccessException)
        {
            return Result<bool>.Fail(ErrorCode.E_IO, exception.Message, directory);
        }

        var files = new (string Name, object Value)[]
        {
            (SampleFiles.Scene, sample.Scene),
            (SampleFiles.Queries, sample.Queries),
            (SampleFiles.GroundTruth, sample.GroundTruth),
            (SampleFiles.Edits, sample.Edits),
            (SampleFiles.EditTruth, sample.EditTruth)
        };

        var manifest = new SampleManifest
        {
            Seed = sample.Scene.Seed,
            SceneContentHash = sample.Scene.Declaration.ContentHash,
            FinalContentHash = sample.EditTruth.FinalContentHash,
            Files = []
        };
        foreach (var (name, value) in files)
        {
            var path = Path.Combine(directory, name);
            var write = SceneJsonCodec.WriteJson(path, value);
            if (!write.IsSuccess)
            {
                return write;
            }

            manifest.Files.Add(new SampleFileHash
            {
                Name = name,
                Sha256 = HashFile(path)
            });
        }

        return SceneJsonCodec.WriteJson(Path.Combine(directory, SampleFiles.Manifest), manifest);
    }

    private static SceneDocument BuildScene(int seed)
    {
        var random = new DeterministicRandom((uint)seed);
        var builder = new SceneBuilder(seed);
        builder.AddNode("root", null, Vector3.Zero, 0, Vector3.One);

        builder.AddNode("rig-left", "root", new Vector3(-5.8f, 0, 0), -8, new Vector3(1, 1, 1));
        builder.AddNode("left-crate-a", "rig-left", new Vector3(-1.7f, 1.1f, 0.3f), 13, new Vector3(0.9f));
        builder.AddAabb("g-left-crate-a", "left-crate-a", new Vector3(-1), new Vector3(1));
        builder.AddNode("left-sphere-a", "rig-left", new Vector3(0.35f, 2.0f, 0.8f), 0, new Vector3(1));
        builder.AddSphere("g-left-sphere-a", "left-sphere-a", Vector3.Zero, 0.95f);
        builder.AddNode("left-mesh-a", "rig-left", new Vector3(-0.5f, 0.5f, 1.8f), -22, new Vector3(0.8f));
        builder.AddMesh("g-left-mesh-a", "left-mesh-a", Pyramid(), PyramidIndices());
        builder.AddNode("left-mesh-tip", "left-mesh-a", new Vector3(0, 1.9f, 0), 18, new Vector3(0.42f));
        builder.AddSphere("g-left-mesh-tip", "left-mesh-tip", Vector3.Zero, 0.8f);

        builder.AddNode("rig-right", "root", new Vector3(5.8f, 0, 0), 7, new Vector3(1));
        builder.AddNode("right-crate-a", "rig-right", new Vector3(1.45f, 1.0f, 0.2f), -16, new Vector3(0.85f));
        builder.AddAabb("g-right-crate-a", "right-crate-a", new Vector3(-1), new Vector3(1));
        builder.AddNode("right-sphere-a", "rig-right", new Vector3(-0.65f, 2.15f, -0.2f), 0, new Vector3(1));
        builder.AddSphere("g-right-sphere-a", "right-sphere-a", Vector3.Zero, 0.85f);
        builder.AddNode("right-mesh-a", "rig-right", new Vector3(0.8f, 0.55f, 1.6f), 24, new Vector3(0.75f));
        builder.AddMesh("g-right-mesh-a", "right-mesh-a", Pyramid(), PyramidIndices());

        builder.AddNode("rig-center", "root", Vector3.Zero, 0, new Vector3(1));
        builder.AddNode("center-crate", "rig-center", new Vector3(0, 1.1f, 0.1f), 0, new Vector3(1.05f, 0.8f, 1.05f));
        builder.AddAabb("g-center-crate", "center-crate", new Vector3(-1), new Vector3(1));
        builder.AddSphere(
            "g-center-orb",
            "center-crate",
            new Vector3(0.65f, 0.85f, 0.45f),
            0.32f);
        builder.AddNode("center-mesh", "rig-center", new Vector3(0, 1.7f, 1.7f), -10, new Vector3(0.8f));
        builder.AddMesh("g-center-mesh", "center-mesh", Pyramid(), PyramidIndices());
        builder.AddNode("center-sphere", "rig-center", new Vector3(0, 3.15f, -0.7f), 0, new Vector3(0.75f));
        builder.AddSphere("g-center-sphere", "center-sphere", Vector3.Zero, 1f);

        builder.AddNode("rig-back", "root", new Vector3(0, 0, -5.5f), 0, new Vector3(1));
        builder.AddNode("back-crate", "rig-back", new Vector3(-2.2f, 0.75f, 0), 12, new Vector3(0.7f));
        builder.AddAabb("g-back-crate", "back-crate", new Vector3(-1), new Vector3(1));
        builder.AddNode("back-sphere", "rig-back", new Vector3(2.1f, 1.3f, -0.2f), 0, new Vector3(1));
        builder.AddSphere("g-back-sphere", "back-sphere", Vector3.Zero, 0.9f);
        builder.AddNode("back-mesh", "rig-back", new Vector3(0, 0.35f, 0.8f), 34, new Vector3(0.9f));
        builder.AddMesh("g-back-mesh", "back-mesh", Pyramid(), PyramidIndices());

        var scene = builder.Build();
        foreach (var geometry in scene.Geometries)
        {
            if (geometry.Kind == "aabb")
            {
                geometry.BoundsMin![0] += (random.NextFloat() - 0.5f) * 0.02f;
            }
        }

        var state = SceneState.FromDocument(scene);
        scene.Declaration = new SceneDeclaration
        {
            NodeCount = scene.Nodes.Count,
            GeometryCount = scene.Geometries.Count,
            ContentHash = state.ContentHash,
            StructureHash = state.StructureHash,
            ParamVersion = SpatialParameterVersion.Current
        };
        return scene;
    }

    private static QueryManifest BuildQueries()
    {
        var rays = new List<RayQueryDefinition>
        {
            new() { Id = "ray-center", ScreenX = 640, ScreenY = 360 },
            new() { Id = "ray-center-up", ScreenX = 640, ScreenY = 260 },
            new() { Id = "ray-center-down", ScreenX = 640, ScreenY = 470 },
            new() { Id = "ray-left", ScreenX = 420, ScreenY = 350 },
            new() { Id = "ray-right", ScreenX = 860, ScreenY = 350 },
            new() { Id = "ray-left-up", ScreenX = 330, ScreenY = 250 },
            new() { Id = "ray-right-up", ScreenX = 950, ScreenY = 250 },
            new() { Id = "ray-left-low", ScreenX = 300, ScreenY = 470 },
            new() { Id = "ray-right-low", ScreenX = 980, ScreenY = 470 },
            new() { Id = "ray-miss-corner", ScreenX = 20, ScreenY = 20 },
            new() { Id = "ray-miss-bottom", ScreenX = 640, ScreenY = 710 },
            new() { Id = "ray-miss-right", ScreenX = 1260, ScreenY = 60 }
        };

        var random = new DeterministicRandom(99181);
        for (var i = 0; i < 4; i++)
        {
            rays.Add(new RayQueryDefinition
            {
                Id = $"ray-random-{i:00}",
                ScreenX = 250 + random.NextFloat() * 780,
                ScreenY = 140 + random.NextFloat() * 420
            });
        }

        var boxes = new List<BoxQueryDefinition>
        {
            new() { Id = "box-left", Rect = [70, 150, 330, 390] },
            new() { Id = "box-right", Rect = [880, 150, 330, 390] },
            new() { Id = "box-center", Rect = [500, 130, 280, 400] },
            new() { Id = "box-full", Rect = [0, 0, 1280, 720] },
            new() { Id = "box-empty-corner", Rect = [0, 0, 20, 20] }
        };

        return new QueryManifest
        {
            Rays = rays,
            Boxes = boxes
        };
    }

    private static EditSequenceDefinition BuildEdits(string baseHash)
    {
        return new EditSequenceDefinition
        {
            BaseContentHash = baseHash,
            Edits =
            [
                new SceneEdit
                {
                    Id = "edit-transform-center-mesh",
                    Kind = "transform",
                    NodeId = "center-mesh",
                    LocalMatrix = SpatialMath.MatrixToRowMajor(
                        SpatialMath.ComposeTRS(
                            new Vector3(0.45f, 1.95f, 1.55f),
                            Quaternion.CreateFromAxisAngle(Vector3.UnitY, 24f * MathF.PI / 180f),
                            new Vector3(0.82f)))
                },
                new SceneEdit
                {
                    Id = "edit-add-left-sphere",
                    Kind = "addGeometry",
                    Geometry = new GeometryDefinition
                    {
                        Id = "g-left-added",
                        NodeId = "rig-left",
                        Kind = "sphere",
                        Center = [2.15f, 0.75f, 1.05f],
                        Radius = 0.58f
                    }
                },
                new SceneEdit
                {
                    Id = "edit-remove-back-mesh",
                    Kind = "removeGeometry",
                    GeometryId = "g-back-mesh"
                },
                new SceneEdit
                {
                    Id = "edit-pivot-translate",
                    Kind = "pivot",
                    Selection = ["rig-left", "left-crate-a"],
                    Pivot = [-5.8f, 1.0f, 0.0f],
                    Operation = "translate",
                    Vector = [0, 0.85f, 0]
                },
                new SceneEdit
                {
                    Id = "edit-pivot-rotate",
                    Kind = "pivot",
                    Selection = ["rig-right", "right-sphere-a"],
                    Pivot = [5.8f, 2.0f, 0],
                    Operation = "rotate",
                    Axis = [0, 1, 0],
                    AngleDegrees = 18f
                },
                new SceneEdit
                {
                    Id = "edit-pivot-scale",
                    Kind = "pivot",
                    Selection = ["rig-center"],
                    Pivot = [0, 1.5f, 0],
                    Operation = "scale",
                    Factor = 1.12f
                }
            ]
        };
    }

    private static EditTruthManifest BuildEditTruth(
        SceneState initialState,
        QueryManifest queries,
        EditSequenceDefinition edits)
    {
        var state = initialState.Clone();
        var steps = new List<EditStepTruth>();
        foreach (var edit in edits.Edits)
        {
            var applied = ReferenceEvaluator.ApplyEdit(state, edit);
            if (!applied.IsSuccess)
            {
                throw new InvalidOperationException(
                    $"Generated edit failed: {applied.Error} (nodes={state.Nodes.Count}: " +
                    $"{string.Join(",", state.Nodes.Keys.OrderBy(item => item, StringComparer.Ordinal))})");
            }

            state = applied.Value!;
            steps.Add(new EditStepTruth
            {
                EditId = edit.Id,
                ContentHash = state.ContentHash,
                StructureVersion = state.StructureVersion,
                Nodes = ReferenceEvaluator.CaptureNodeTruth(state)
            });
        }

        return new EditTruthManifest
        {
            BaseContentHash = initialState.ContentHash,
            FinalContentHash = state.ContentHash,
            Steps = steps,
            FinalNodes = ReferenceEvaluator.CaptureNodeTruth(state),
            FinalRays = ReferenceEvaluator.BuildRayTruth(state, queries),
            FinalBoxes = ReferenceEvaluator.BuildBoxTruth(state, queries)
        };
    }

    private static float[][] Pyramid() =>
    [
        [-1, 0, -1],
        [1, 0, -1],
        [1, 0, 1],
        [-1, 0, 1],
        [0, 2, 0]
    ];

    private static int[] PyramidIndices() =>
    [
        0, 1, 2,
        0, 2, 3,
        0, 1, 4,
        1, 2, 4,
        2, 3, 4,
        3, 0, 4
    ];

    private static string HashFile(string path)
    {
        using var stream = File.OpenRead(path);
        return Convert.ToHexString(SHA256.HashData(stream)).ToLowerInvariant();
    }

    private sealed class SceneBuilder
    {
        private readonly SceneDocument _scene = new();

        public SceneBuilder(int seed)
        {
            _scene.Seed = seed;
            _scene.Camera = new CameraDefinition
            {
                Position = [0, 7.5f, 20.5f],
                Target = [0, 1.7f, 0],
                Up = [0, 1, 0],
                FieldOfViewYDegrees = 50,
                NearPlane = 0.1f,
                FarPlane = 120,
                ViewportWidth = 1280,
                ViewportHeight = 720
            };
        }

        public void AddNode(
            string id,
            string? parent,
            Vector3 translation,
            float yawDegrees,
            Vector3 scale)
        {
            _scene.Nodes.Add(new SceneNodeDefinition
            {
                Id = id,
                ParentId = parent,
                LocalMatrix = SpatialMath.MatrixToRowMajor(
                    SpatialMath.ComposeTRS(
                        translation,
                        Quaternion.CreateFromAxisAngle(
                            Vector3.UnitY,
                            yawDegrees * MathF.PI / 180f),
                        scale))
            });
        }

        public void AddAabb(string id, string nodeId, Vector3 min, Vector3 max)
        {
            _scene.Geometries.Add(new GeometryDefinition
            {
                Id = id,
                NodeId = nodeId,
                Kind = "aabb",
                BoundsMin = SpatialMath.VectorToArray(min),
                BoundsMax = SpatialMath.VectorToArray(max)
            });
        }

        public void AddSphere(string id, string nodeId, Vector3 center, float radius)
        {
            _scene.Geometries.Add(new GeometryDefinition
            {
                Id = id,
                NodeId = nodeId,
                Kind = "sphere",
                Center = SpatialMath.VectorToArray(center),
                Radius = radius
            });
        }

        public void AddMesh(string id, string nodeId, float[][] vertices, int[] indices)
        {
            _scene.Geometries.Add(new GeometryDefinition
            {
                Id = id,
                NodeId = nodeId,
                Kind = "triangleMesh",
                Vertices = vertices,
                Indices = indices
            });
        }

        public SceneDocument Build()
        {
            _scene.Format = SceneFormats.Scene;
            return _scene;
        }
    }

    private sealed class DeterministicRandom
    {
        private uint _state;

        public DeterministicRandom(uint seed)
        {
            _state = seed == 0 ? 0x9E3779B9u : seed;
        }

        public uint NextUInt()
        {
            var value = _state;
            value ^= value << 13;
            value ^= value >> 17;
            value ^= value << 5;
            _state = value;
            return value;
        }

        public float NextFloat() => (NextUInt() & 0x00FFFFFF) / 16777216f;
    }
}

public sealed class SampleManifest
{
    public int Seed { get; set; }
    public string SceneContentHash { get; set; } = string.Empty;
    public string FinalContentHash { get; set; } = string.Empty;
    public List<SampleFileHash> Files { get; set; } = [];
}

public sealed class SampleFileHash
{
    public string Name { get; set; } = string.Empty;
    public string Sha256 { get; set; } = string.Empty;
}
