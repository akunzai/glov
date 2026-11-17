using System;
using System.Runtime.InteropServices;

namespace HelloFramework;

// Minimal .NET Framework app for checking a guest's build-and-run path:
// stdout and stderr stay separate, and `--exit <code>` sets the exit code.
internal static class Program
{
    private static int Main(string[] args)
    {
        var exitCode = args.Length == 2 && args[0] == "--exit" ? int.Parse(args[1]) : 0;
        Console.WriteLine($"Hello from .NET Framework {Environment.Version} ({RuntimeInformation.FrameworkDescription})");
        Console.WriteLine($"OS {RuntimeInformation.OSDescription.Trim()}, process {RuntimeInformation.ProcessArchitecture}");
        Console.Error.WriteLine($"stderr check, exiting with {exitCode}");
        return exitCode;
    }
}
