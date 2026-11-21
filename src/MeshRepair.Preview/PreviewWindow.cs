using OpenTK.Graphics.OpenGL4;
using OpenTK.Mathematics;
using OpenTK.Windowing.Common;
using OpenTK.Windowing.Desktop;
using MeshRepair.Core.Geometry;
using MeshRepair.Core.Versioning;

namespace MeshRepair.Preview;

public sealed class PreviewWindow : GameWindow
{
    private readonly MeshRepairRoom _room;
    private int _program;
    private int _triangleVao;
    private int _triangleVbo;
    private int _lineVao;
    private int _lineVbo;
    private int _matrixLocation;
    private int _triangleCount;
    private int _lineVertexCount;
    private long _loadedVersion = -1;
    private Matrix4 _projection;

    public PreviewWindow(MeshRepairRoom room, GameWindowSettings gameSettings, NativeWindowSettings nativeSettings)
        : base(gameSettings, nativeSettings)
    {
        _room = room;
    }

    protected override void OnLoad()
    {
        base.OnLoad();
        GL.ClearColor(0.055f, 0.07f, 0.1f, 1f);
        _program = CreateProgram();
        _matrixLocation = GL.GetUniformLocation(_program, "uProjection");
        _triangleVao = GL.GenVertexArray();
        _triangleVbo = GL.GenBuffer();
        _lineVao = GL.GenVertexArray();
        _lineVbo = GL.GenBuffer();
        _projection = Matrix4.Identity;
    }

    protected override void OnRenderFrame(FrameEventArgs args)
    {
        base.OnRenderFrame(args);
        var acquired = _room.AcquireSnapshot();
        if (!acquired.IsSuccess)
        {
            return;
        }

        using var handle = acquired.RequireValue();
        var read = handle.Read();
        if (!read.IsSuccess)
        {
            return;
        }

        var version = read.RequireValue();
        if (version.Version != _loadedVersion)
        {
            Upload(PreviewGeometryBuilder.Build(version));
            _loadedVersion = version.Version;
        }

        GL.Clear(ClearBufferMask.ColorBufferBit | ClearBufferMask.DepthBufferBit);
        GL.UseProgram(_program);
        GL.UniformMatrix4(_matrixLocation, false, ref _projection);
        GL.BindVertexArray(_triangleVao);
        GL.DrawArrays(PrimitiveType.Triangles, 0, _triangleCount);
        if (_lineVertexCount > 0)
        {
            GL.BindVertexArray(_lineVao);
            GL.DrawArrays(PrimitiveType.Lines, 0, _lineVertexCount);
        }

        SwapBuffers();
    }

    protected override void OnResize(ResizeEventArgs e)
    {
        base.OnResize(e);
        GL.Viewport(0, 0, e.Width, e.Height);
        var acquired = _room.AcquireSnapshot();
        if (!acquired.IsSuccess)
        {
            return;
        }

        using var handle = acquired.RequireValue();
        var read = handle.Read();
        if (!read.IsSuccess)
        {
            return;
        }

        var geometry = PreviewGeometryBuilder.Build(read.RequireValue());
        var bounds = geometry.Bounds;
        var margin = 0.5;
        _projection = Matrix4.CreateOrthographicOffCenter(
            (float)(bounds.Min.X - margin),
            (float)(bounds.Max.X + margin),
            (float)(bounds.Min.Y - margin),
            (float)(bounds.Max.Y + margin),
            -10,
            10);
    }

    protected override void OnUnload()
    {
        GL.DeleteBuffer(_triangleVbo);
        GL.DeleteVertexArray(_triangleVao);
        GL.DeleteBuffer(_lineVbo);
        GL.DeleteVertexArray(_lineVao);
        GL.DeleteProgram(_program);
        base.OnUnload();
    }

    private void Upload(PreviewGeometry geometry)
    {
        var triangleData = geometry.Triangles
            .SelectMany(vertex => new[]
            {
                (float)vertex.Position.X,
                (float)vertex.Position.Y,
                (float)vertex.Position.Z,
                ((vertex.PackedColor >> 16) & 0xFF) / 255f,
                ((vertex.PackedColor >> 8) & 0xFF) / 255f,
                (vertex.PackedColor & 0xFF) / 255f
            })
            .ToArray();
        GL.BindVertexArray(_triangleVao);
        GL.BindBuffer(BufferTarget.ArrayBuffer, _triangleVbo);
        GL.BufferData(BufferTarget.ArrayBuffer, triangleData.Length * sizeof(float), triangleData, BufferUsageHint.DynamicDraw);
        GL.EnableVertexAttribArray(0);
        GL.VertexAttribPointer(0, 3, VertexAttribPointerType.Float, false, 6 * sizeof(float), 0);
        GL.EnableVertexAttribArray(1);
        GL.VertexAttribPointer(1, 3, VertexAttribPointerType.Float, false, 6 * sizeof(float), 3 * sizeof(float));
        _triangleCount = geometry.Triangles.Count;

        var lineData = geometry.BoundaryLines
            .SelectMany(line => new[]
            {
                line.A,
                line.B
            })
            .SelectMany(point => new[]
            {
                (float)point.X,
                (float)point.Y,
                (float)point.Z,
                ((geometry.BoundaryLines[0].PackedColor >> 16) & 0xFF) / 255f,
                ((geometry.BoundaryLines[0].PackedColor >> 8) & 0xFF) / 255f,
                (geometry.BoundaryLines[0].PackedColor & 0xFF) / 255f
            })
            .ToArray();
        GL.BindVertexArray(_lineVao);
        GL.BindBuffer(BufferTarget.ArrayBuffer, _lineVbo);
        GL.BufferData(BufferTarget.ArrayBuffer, lineData.Length * sizeof(float), lineData, BufferUsageHint.DynamicDraw);
        GL.EnableVertexAttribArray(0);
        GL.VertexAttribPointer(0, 3, VertexAttribPointerType.Float, false, 6 * sizeof(float), 0);
        GL.EnableVertexAttribArray(1);
        GL.VertexAttribPointer(1, 3, VertexAttribPointerType.Float, false, 6 * sizeof(float), 3 * sizeof(float));
        _lineVertexCount = lineData.Length / 6;
    }

    private static int CreateProgram()
    {
        const string vertexShader = """
            #version 330 core
            layout (location = 0) in vec3 aPosition;
            layout (location = 1) in vec3 aColor;
            uniform mat4 uProjection;
            out vec3 vColor;
            void main() {
                vColor = aColor;
                gl_Position = uProjection * vec4(aPosition, 1.0);
            }
            """;
        const string fragmentShader = """
            #version 330 core
            in vec3 vColor;
            out vec4 FragColor;
            void main() {
                FragColor = vec4(vColor, 1.0);
            }
            """;
        var vertex = Compile(ShaderType.VertexShader, vertexShader);
        var fragment = Compile(ShaderType.FragmentShader, fragmentShader);
        var program = GL.CreateProgram();
        GL.AttachShader(program, vertex);
        GL.AttachShader(program, fragment);
        GL.LinkProgram(program);
        GL.GetProgram(program, GetProgramParameterName.LinkStatus, out var linked);
        if (linked == 0)
        {
            throw new InvalidOperationException(GL.GetProgramInfoLog(program));
        }

        GL.DeleteShader(vertex);
        GL.DeleteShader(fragment);
        return program;
    }

    private static int Compile(ShaderType type, string source)
    {
        var shader = GL.CreateShader(type);
        GL.ShaderSource(shader, source);
        GL.CompileShader(shader);
        GL.GetShader(shader, ShaderParameter.CompileStatus, out var compiled);
        if (compiled == 0)
        {
            throw new InvalidOperationException(GL.GetShaderInfoLog(shader));
        }

        return shader;
    }
}
