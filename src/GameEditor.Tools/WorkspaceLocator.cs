namespace GameEditor.Tools;

public static class WorkspaceLocator
{
    public static string FindRoot()
    {
        var candidates = new[]
        {
            Directory.GetCurrentDirectory(),
            AppContext.BaseDirectory
        };
        foreach (var candidate in candidates)
        {
            var directory = new DirectoryInfo(Path.GetFullPath(candidate));
            while (directory is not null)
            {
                if (File.Exists(Path.Combine(directory.FullName, "GameEditor.sln")))
                {
                    return directory.FullName;
                }

                directory = directory.Parent;
            }
        }

        return Directory.GetCurrentDirectory();
    }
}
