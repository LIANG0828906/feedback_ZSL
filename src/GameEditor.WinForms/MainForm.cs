using System.Numerics;
using GameEditor.Spatial;

namespace GameEditor.WinForms;

internal sealed class MainForm : Form
{
    private readonly SceneView _sceneView = new() { Dock = DockStyle.Fill };
    private readonly Label _status = new() { AutoSize = true, ForeColor = Color.FromArgb(210, 220, 230) };
    private readonly Label _selectionLabel = new() { AutoSize = true, ForeColor = Color.FromArgb(210, 220, 230) };
    private readonly ComboBox _operation = new()
    {
        DropDownStyle = ComboBoxStyle.DropDownList,
        Width = 180
    };
    private readonly NumericUpDown _amount = new()
    {
        DecimalPlaces = 2,
        Minimum = -360,
        Maximum = 360,
        Increment = 0.1m,
        Value = 0.5m,
        Width = 180
    };
    private readonly NumericUpDown _pivotX = new() { DecimalPlaces = 2, Minimum = -1000, Maximum = 1000, Width = 62 };
    private readonly NumericUpDown _pivotY = new() { DecimalPlaces = 2, Minimum = -1000, Maximum = 1000, Width = 62 };
    private readonly NumericUpDown _pivotZ = new() { DecimalPlaces = 2, Minimum = -1000, Maximum = 1000, Width = 62 };
    private readonly Button _previewButton = new() { Text = "Preview operation", AutoSize = true };
    private readonly Button _commitButton = new() { Text = "Commit batch", AutoSize = true, Enabled = false };
    private readonly Button _cancelButton = new() { Text = "Cancel preview", AutoSize = true, Enabled = false };
    private readonly Button _addGeometryButton = new() { Text = "Add sphere to selection", AutoSize = true };
    private readonly Button _removeGeometryButton = new() { Text = "Remove first selected geometry", AutoSize = true };
    private SpatialRuntime? _runtime;
    private QueryManifest? _queries;
    private PivotOperationSession? _session;
    private IReadOnlyList<string> _selection = [];
    private int _addedGeometryCounter;

    public MainForm()
    {
        Text = "GameEditor.Spatial - deterministic picking and pivot operations";
        Width = 1360;
        Height = 860;
        MinimumSize = new Size(1000, 640);
        StartPosition = FormStartPosition.CenterScreen;
        BackColor = Color.FromArgb(30, 35, 42);

        var toolbar = new FlowLayoutPanel
        {
            Dock = DockStyle.Top,
            Height = 42,
            Padding = new Padding(8),
            BackColor = Color.FromArgb(38, 44, 53),
            WrapContents = false
        };
        var reload = new Button { Text = "Reload sample", AutoSize = true };
        var generate = new Button { Text = "Regenerate sample", AutoSize = true };
        reload.Click += (_, _) => LoadSample(reload: true);
        generate.Click += (_, _) => RegenerateSample();
        toolbar.Controls.Add(reload);
        toolbar.Controls.Add(generate);
        toolbar.Controls.Add(new Label
        {
            Text = "   Click/drag in the viewport. Pivot operations use only topmost selected nodes.",
            AutoSize = true,
            ForeColor = Color.FromArgb(200, 212, 225),
            Padding = new Padding(0, 8, 0, 0)
        });

        var side = new FlowLayoutPanel
        {
            Dock = DockStyle.Left,
            Width = 330,
            Padding = new Padding(14),
            FlowDirection = FlowDirection.TopDown,
            WrapContents = false,
            AutoScroll = true,
            BackColor = Color.FromArgb(27, 32, 39)
        };
        AddHeading(side, "Selection");
        _selectionLabel.MaximumSize = new Size(290, 0);
        side.Controls.Add(_selectionLabel);
        side.Controls.Add(new Label { Height = 8 });
        AddHeading(side, "Pivot point (world)");
        var pivotRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false };
        pivotRow.Controls.AddRange(
        [
            new Label { Text = "X", AutoSize = true, Padding = new Padding(0, 6, 0, 0) },
            _pivotX,
            new Label { Text = "Y", AutoSize = true, Padding = new Padding(6, 6, 0, 0) },
            _pivotY,
            new Label { Text = "Z", AutoSize = true, Padding = new Padding(6, 6, 0, 0) },
            _pivotZ
        ]);
        side.Controls.Add(pivotRow);
        var useCenter = new Button { Text = "Use selection bounds center", AutoSize = true };
        useCenter.Click += (_, _) => SetPivotFromSelection();
        side.Controls.Add(useCenter);

        AddHeading(side, "Pivot operation");
        _operation.Items.AddRange(["Translate", "Rotate around Y", "Scale"]);
        _operation.SelectedIndex = 0;
        side.Controls.Add(_operation);
        side.Controls.Add(new Label
        {
            Text = "Amount: translation units / degrees / factor",
            AutoSize = true,
            ForeColor = Color.FromArgb(180, 194, 208)
        });
        side.Controls.Add(_amount);
        var applyRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false };
        applyRow.Controls.AddRange([_previewButton, _commitButton, _cancelButton]);
        side.Controls.Add(applyRow);

        AddHeading(side, "Incremental edits");
        side.Controls.Add(_addGeometryButton);
        side.Controls.Add(_removeGeometryButton);

        AddHeading(side, "Status");
        _status.MaximumSize = new Size(290, 0);
        side.Controls.Add(_status);
        side.Controls.Add(new Label
        {
            Text = "The sample is deterministic. Every preview is a working-copy session; only Commit writes a batch and rebuilds dependent cache entries.",
            AutoSize = true,
            MaximumSize = new Size(290, 0),
            ForeColor = Color.FromArgb(150, 166, 182)
        });

        var split = new SplitContainer
        {
            Dock = DockStyle.Fill,
            FixedPanel = FixedPanel.Panel1
        };
        split.Panel1.Controls.Add(side);
        split.Panel2.Controls.Add(_sceneView);

        Controls.Add(split);
        Controls.Add(toolbar);
        Shown += (_, _) =>
        {
            split.Panel1MinSize = 280;
            split.Panel2MinSize = 500;
            split.SplitterDistance = 330;
        };

        _sceneView.SelectionChanged += (_, selection) =>
        {
            if (_session is not null)
            {
                _session.Cancel();
                _session = null;
                _commitButton.Enabled = false;
                _cancelButton.Enabled = false;
                _sceneView.SetPreview(null, null);
            }

            _selection = selection;
            _selectionLabel.Text = selection.Count == 0
                ? "<none>"
                : string.Join(", ", selection.OrderBy(item => item, StringComparer.Ordinal));
        };
        _sceneView.StatusChanged += (_, text) => _status.Text = text;
        _previewButton.Click += (_, _) => PreviewOperation();
        _commitButton.Click += (_, _) => CommitOperation();
        _cancelButton.Click += (_, _) => CancelOperation();
        _addGeometryButton.Click += (_, _) => AddGeometry();
        _removeGeometryButton.Click += (_, _) => RemoveGeometry();

        LoadSample(reload: false);
    }

    private void LoadSample(bool reload)
    {
        try
        {
            var root = WorkspaceLocator.FindRoot();
            var directory = Path.Combine(root, "samples", "default");
            var scenePath = Path.Combine(directory, SampleFiles.Scene);
            if (!File.Exists(scenePath))
            {
                var generated = DeterministicSceneGenerator.Generate();
                var write = DeterministicSceneGenerator.Write(generated, directory);
                if (!write.IsSuccess)
                {
                    ShowError(write.Error);
                    return;
                }
            }

            var sceneResult = SceneJsonCodec.LoadScene(scenePath);
            if (!sceneResult.IsSuccess)
            {
                ShowError(sceneResult.Error);
                return;
            }

            var queryResult = SceneJsonCodec.LoadQueries(
                Path.Combine(directory, SampleFiles.Queries),
                sceneResult.Value!);
            if (!queryResult.IsSuccess)
            {
                ShowError(queryResult.Error);
                return;
            }

            var runtimeResult = SpatialRuntime.Create(sceneResult.Value!);
            if (!runtimeResult.IsSuccess)
            {
                ShowError(runtimeResult.Error);
                return;
            }

            _runtime = runtimeResult.Value!;
            _queries = queryResult.Value!;
            _session = null;
            _selection = [];
            _sceneView.SetRuntime(_runtime);
            _sceneView.SetSelection([]);
            _selectionLabel.Text = "<none>";
            _status.Text = $"Loaded {sceneResult.Value!.Nodes.Count} nodes / {sceneResult.Value.Geometries.Count} geometries.";
            _commitButton.Enabled = false;
            _cancelButton.Enabled = false;
            SetPivotFromSelection();
        }
        catch (Exception exception)
        {
            ShowError(new DomainError(ErrorCode.E_INTERNAL, exception.Message));
        }
    }

    private void RegenerateSample()
    {
        var sample = DeterministicSceneGenerator.Generate();
        var path = Path.Combine(WorkspaceLocator.FindRoot(), "samples", "default");
        var write = DeterministicSceneGenerator.Write(sample, path);
        if (!write.IsSuccess)
        {
            ShowError(write.Error);
            return;
        }

        LoadSample(reload: true);
    }

    private void PreviewOperation()
    {
        if (_runtime is null || _selection.Count == 0)
        {
            _status.Text = "Select at least one node first.";
            return;
        }

        if (_session is null)
        {
            var pivot = new Vector3(
                (float)_pivotX.Value,
                (float)_pivotY.Value,
                (float)_pivotZ.Value);
            var created = _runtime.BeginPivotSession(_selection, pivot);
            if (!created.IsSuccess)
            {
                ShowError(created.Error);
                return;
            }

            _session = created.Value;
        }

        var amount = (float)_amount.Value;
        var operation = _operation.SelectedIndex switch
        {
            0 => PivotOperation.Translate(new Vector3(0, amount, 0)),
            1 => PivotOperation.Rotate(Vector3.UnitY, amount),
            _ => PivotOperation.Scale(MathF.Max(0.01f, amount))
        };
        var applied = _session!.Apply(operation);
        if (!applied.IsSuccess)
        {
            ShowError(applied.Error);
            return;
        }

        _sceneView.SetPreview(
            _session.PreviewWorldTransforms(),
            new Vector3((float)_pivotX.Value, (float)_pivotY.Value, (float)_pivotZ.Value));
        _commitButton.Enabled = true;
        _cancelButton.Enabled = true;
        _status.Text = "Working-copy preview active. Runtime state is unchanged until Commit.";
    }

    private void CommitOperation()
    {
        if (_session is null)
        {
            return;
        }

        var result = _session.Commit();
        if (!result.IsSuccess)
        {
            ShowError(result.Error);
            return;
        }

        _status.Text =
            $"Committed revision {result.Value!.Revision}. " +
            $"Invalidated {result.Value.Invalidations.Count}, rebuilt {result.Value.Stats.RebuiltNodes} node cache(s).";
        _session = null;
        _commitButton.Enabled = false;
        _cancelButton.Enabled = false;
        _sceneView.SetPreview(null, null);
        _runtime = RequireRuntime();
        _sceneView.SetRuntime(_runtime);
        _sceneView.SetSelection(_selection);
    }

    private void CancelOperation()
    {
        _session?.Cancel();
        _session = null;
        _commitButton.Enabled = false;
        _cancelButton.Enabled = false;
        _sceneView.SetPreview(null, null);
        _sceneView.SetRuntime(RequireRuntime());
        _sceneView.SetSelection(_selection);
        _status.Text = "Working-copy preview canceled.";
    }

    private void AddGeometry()
    {
        if (_runtime is null || _selection.Count == 0)
        {
            _status.Text = "Select an owner node first.";
            return;
        }

        var nodeId = _selection.OrderBy(item => item, StringComparer.Ordinal).First();
        var geometry = new GeometryDefinition
        {
            Id = $"ui-sphere-{_addedGeometryCounter++:000}",
            NodeId = nodeId,
            Kind = "sphere",
            Center = [0, 0, 0],
            Radius = 0.35f
        };
        var result = _runtime.ApplyAddGeometry(geometry);
        if (!result.IsSuccess)
        {
            ShowError(result.Error);
            return;
        }

        _status.Text =
            $"Added {geometry.Id}; rebuilt {result.Value!.Stats.RebuiltEntries} entry, " +
            $"reused {result.Value.Stats.ReusedEntries} entries.";
        _sceneView.SetRuntime(_runtime);
        _sceneView.SetSelection(_selection);
    }

    private void RemoveGeometry()
    {
        if (_runtime is null || _selection.Count == 0)
        {
            _status.Text = "Select a node with geometry first.";
            return;
        }

        var nodeId = _selection.OrderBy(item => item, StringComparer.Ordinal).First();
        var render = _runtime.GetRenderGeometry();
        var geometry = render.Value!.FirstOrDefault(item => item.NodeId == nodeId);
        if (geometry is null)
        {
            _status.Text = $"Node {nodeId} has no removable geometry.";
            return;
        }

        var result = _runtime.ApplyRemoveGeometry(geometry.GeometryId);
        if (!result.IsSuccess)
        {
            ShowError(result.Error);
            return;
        }

        _status.Text =
            $"Removed {geometry.GeometryId}; rebuilt {result.Value!.Stats.RebuiltEntries} entry, " +
            $"reused {result.Value.Stats.ReusedEntries} entries.";
        _sceneView.SetRuntime(_runtime);
        _sceneView.SetSelection(_selection);
    }

    private void SetPivotFromSelection()
    {
        if (_runtime is null || _selection.Count == 0)
        {
            return;
        }

        var snapshot = _runtime.CaptureSnapshot();
        var bounds = Bounds3.Empty;
        foreach (var nodeId in _selection)
        {
            var node = snapshot.Nodes.FirstOrDefault(item => item.NodeId == nodeId);
            if (node is null)
            {
                continue;
            }

            bounds = node.WorldBounds.IsEmpty
                ? bounds.Encapsulate(new Vector3(
                    node.WorldTransform.M41,
                    node.WorldTransform.M42,
                    node.WorldTransform.M43))
                : bounds.Encapsulate(node.WorldBounds);
        }

        if (!bounds.IsEmpty)
        {
            var center = bounds.Center;
            _pivotX.Value = ClampNumeric(center.X);
            _pivotY.Value = ClampNumeric(center.Y);
            _pivotZ.Value = ClampNumeric(center.Z);
        }
    }

    private SpatialRuntime RequireRuntime() =>
        _runtime ?? throw new InvalidOperationException("Runtime is not loaded.");

    private static decimal ClampNumeric(float value) =>
        Math.Clamp((decimal)value, -1000m, 1000m);

    private static void AddHeading(Control parent, string text)
    {
        parent.Controls.Add(new Label
        {
            Text = text,
            AutoSize = true,
            Font = new Font("Segoe UI Semibold", 10.5f),
            ForeColor = Color.FromArgb(235, 240, 246),
            Margin = new Padding(0, 14, 0, 4)
        });
    }

    private void ShowError(DomainError? error)
    {
        _status.Text = error?.ToString() ?? "Unknown error.";
        MessageBox.Show(this, _status.Text, "GameEditor.Spatial", MessageBoxButtons.OK, MessageBoxIcon.Error);
    }
}
