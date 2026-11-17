# The Windows x64 release archive: glov.exe, glovd.exe, glov-broker.exe and the Linux Guest Service, in one folder
# that `glov host install` and `glov host install-broker` install from, plus its SHA-256 file. Prints the archive's
# path. Run with pwsh on Windows.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
Set-Location (Split-Path -Parent $PSScriptRoot)

mise run windows:build | Out-Host
if ($LASTEXITCODE -ne 0) { throw "mise run windows:build failed ($LASTEXITCODE)" }

$version = (Get-Content -Raw VERSION).Trim()
$name = "glov-$version-windows-x64"
$dist = Join-Path (Get-Location) '.build\dist'
$stage = Join-Path $dist $name
$zip = Join-Path $dist "$name.zip"
foreach ($path in $stage, $zip, "$zip.sha256") {
    if (Test-Path $path) { Remove-Item -Recurse -Force $path }
}
New-Item -ItemType Directory -Force $stage | Out-Null
foreach ($file in 'glov.exe', 'glovd.exe', 'glov-broker.exe', 'glov-guest-linux-amd64') {
    Copy-Item (Join-Path '.build\windows' $file) $stage
}
Copy-Item LICENSE $stage
python -B tools/third-party-notices.py -o (Join-Path $stage 'THIRD_PARTY_NOTICES.md') --go guest:linux --go windows:windows
if ($LASTEXITCODE -ne 0) { throw "tools/third-party-notices.py failed ($LASTEXITCODE)" }

# Entries keep the folder name, as the macOS archive does, and .NET writes them with forward slashes.
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::CreateFromDirectory($stage, $zip, [IO.Compression.CompressionLevel]::Optimal, $true)

# The same line shasum -a 256 writes, so either side can check it.
$hash = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$zip.sha256", "$hash  $name.zip`n")
Write-Output $zip
