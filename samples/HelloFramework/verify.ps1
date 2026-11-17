# Builds HelloFramework with the VS Build Tools MSBuild and checks its output and exit code.
# Run in a Windows guest: powershell -ExecutionPolicy Bypass -File verify.ps1
# Windows PowerShell turns native stderr into errors under Stop; every step checks its exit code instead.
$ErrorActionPreference = 'Continue'
$vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
$msbuild = & $vswhere -latest -products * -requires Microsoft.Component.MSBuild -find 'MSBuild\**\Bin\arm64\MSBuild.exe' | Select-Object -First 1
if (!$msbuild) { throw 'MSBuild not found; install VS Build Tools with --includeRecommended' }

& $msbuild "$PSScriptRoot\HelloFramework.csproj" -restore -noLogo -verbosity:minimal -p:Configuration=Release
if ($LASTEXITCODE -ne 0) { throw "build failed: $LASTEXITCODE" }

$exe = "$PSScriptRoot\bin\Release\net48\HelloFramework.exe"
$out = & $exe --exit 3 2>$null
if ($LASTEXITCODE -ne 3) { throw "expected exit code 3, got $LASTEXITCODE" }
if ($out[0] -notmatch '^Hello from \.NET Framework') { throw "unexpected output: $out" }
$out
'verify: ok'
