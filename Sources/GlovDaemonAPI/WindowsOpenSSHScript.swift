// setup-openssh-server.ps1, written next to setup.ps1 on the unattend image of a VM created with --ssh. It is an unmodified
// copy of https://gist.github.com/akunzai/1a61e403f38948fcdbcaf5f139be0728 at revision 5518803: update it from there, byte
// for byte, together with its SHA-256 in WindowsProvisionerTests.

extension WindowsAssets {
    static let openSSHScript = ##"""
<#
.SYNOPSIS
  Quickly install and configure Windows OpenSSH Server with public key authentication and strict ACLs.
.DESCRIPTION
  Supports Windows 10/11 and Windows Server 2019/2022/2025:
  1. Checks Administrator elevation.
  2. Supports uninstallation (-Uninstall): stops services, removes Feature on Demand (FOD) or Win32-OpenSSH service, and cleans firewall rules (-PurgeData to purge config/keys).
  3. Supports dual installation modes:
     - Default mode (native): Installs OpenSSH.Server via Feature on Demand (FOD).
     - Standalone mode (-UseWin32OpenSSH): Downloads official Win32-OpenSSH from GitHub (auto-selected on Server 2019; also avoids Server 2025 FOD 9.5.2.1 ACL crash bugs).
  4. Supports standalone ACL repair (-FixPermissions) to resolve Error 1067 / Event ID 7034.
  5. Sets sshd service startup type to Automatic and starts the service.
  6. Automatically checks and enables Windows Defender Firewall inbound rules (OpenSSH-Server-In-TCP).
  7. Deploys public keys with strict ACLs using Well-Known SIDs (*S-1-5-18 and *S-1-5-32-544) for cross-language Windows compatibility:
     - Administrator (default): %ProgramData%\ssh\administrators_authorized_keys (SYSTEM and Administrators Full Control only).
     - Specific user (-ForUser <username>): C:\Users\<username>\.ssh\authorized_keys (User and SYSTEM Full Control only).
     - If no key is provided, creates an empty authorized keys file with verified ACLs.
  8. Provides option to disable password authentication (-DisablePasswordAuth).
  9. Supports skipping installation (-SkipInstall, or auto-skipped if sshd already exists when providing a key).
.EXAMPLE
  # Install, start service, and configure empty authorized keys ACL
  .\setup-openssh-server.ps1

  # Use official standalone Win32-OpenSSH release from GitHub
  .\setup-openssh-server.ps1 -UseWin32OpenSSH

  # Fix folder and key ACL permissions and restart sshd (fixes Error 1067 / Event 7034)
  .\setup-openssh-server.ps1 -FixPermissions

  # Uninstall OpenSSH Server (preserves ProgramData\ssh configuration and keys)
  .\setup-openssh-server.ps1 -Uninstall

  # Completely uninstall OpenSSH Server and purge ProgramData\ssh
  .\setup-openssh-server.ps1 -Uninstall -PurgeData

  # Skip installation and append public key to existing OpenSSH server
  .\setup-openssh-server.ps1 -SkipInstall -PublicKey "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5... user@host"

  # Pass public key directly (auto-skips install if server already exists)
  .\setup-openssh-server.ps1 -PublicKey "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5... user@host"

  # Read public key from file
  .\setup-openssh-server.ps1 -KeyFilePath "id_ed25519.pub"

  # Configure authorized_keys for specific standard user
  .\setup-openssh-server.ps1 -PublicKey "ssh-ed25519 ..." -ForUser "alice"

  # Deploy public key and disable password authentication
  .\setup-openssh-server.ps1 -PublicKey "ssh-ed25519 ..." -DisablePasswordAuth
#>
[CmdletBinding()]
param(
  [Parameter(Position = 0)]
  [string]$PublicKey = '',

  [Parameter()]
  [string]$KeyFilePath = '',

  [Parameter()]
  [string]$ForUser = '',

  [Parameter()]
  [switch]$DisablePasswordAuth,

  [Parameter()]
  [int]$Port = 22,

  [Parameter()]
  [switch]$UseWin32OpenSSH,

  [Parameter()]
  [switch]$SkipInstall,

  [Parameter()]
  [switch]$Uninstall,

  [Parameter()]
  [switch]$PurgeData,

  [Parameter()]
  [switch]$FixPermissions
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# 1. Check Administrator elevation
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
  Write-Error "This script must be executed with Administrator privileges in PowerShell!"
}

# Check if sshd service is already present
$existingSshdService = Get-Service -Name sshd -ErrorAction SilentlyContinue
$win32InstallDir = "$env:ProgramFiles\OpenSSH"
$isWin32Installed = (Test-Path "$win32InstallDir\sshd.exe")

# Resolve OpenSSH binary directory
$openSshBinDir = if ($isWin32Installed) {
  $win32InstallDir
} elseif ($existingSshdService) {
  try {
    $svcPath = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\sshd").ImagePath
    if ($svcPath) {
      $resolvedExe = ($svcPath -replace '"', '').Trim()
      Split-Path -Parent $resolvedExe
    } else {
      "$env:SystemRoot\System32\OpenSSH"
    }
  } catch {
    "$env:SystemRoot\System32\OpenSSH"
  }
} else {
  "$env:SystemRoot\System32\OpenSSH"
}

# Core ACL repair function (Strict SDDL, logs directory permission, host keys and authorized keys ACL)
function Repair-OpenSshPermissions {
  [CmdletBinding()]
  param(
    [string]$BinDir = "$env:SystemRoot\System32\OpenSSH"
  )

  Write-Host "=== Repairing OpenSSH Directory and Key ACL Permissions ===" -ForegroundColor Cyan
  $sshDataDir = "$env:ProgramData\ssh"
  if (-not (Test-Path $sshDataDir)) {
    New-Item -ItemType Directory -Path $sshDataDir -Force | Out-Null
  }

  # Ensure host keys exist
  $sshKeygenExe = Join-Path $BinDir "ssh-keygen.exe"
  if (-not (Test-Path $sshKeygenExe)) { $sshKeygenExe = "ssh-keygen" }
  $hostKeys = Get-ChildItem -Path $sshDataDir -Filter "ssh_host_*_key" -ErrorAction SilentlyContinue
  if (-not $hostKeys) {
    Write-Host "Generating OpenSSH host keys (ssh-keygen -A)..." -ForegroundColor Yellow
    try {
      & $sshKeygenExe -A | Out-Null
    } catch {
      Write-Warning "Failed to execute ssh-keygen -A: $($_.Exception.Message)"
    }
  }

  # Apply Microsoft recommended SDDL (SYSTEM and BUILTIN\Administrators Full Control, Authenticated Users Read)
  # Uses Well-Known SIDs to ensure compatibility with non-English Windows locales (SYSTEM: *S-1-5-18, Administrators: *S-1-5-32-544)
  try {
    $sddl = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;AU)"
    $secDesc = New-Object System.Security.AccessControl.RawSecurityDescriptor $sddl

    foreach ($dir in @($sshDataDir, "$sshDataDir\logs")) {
      if (-not (Test-Path $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
      }
      $acl = Get-Acl -Path $dir
      $acl.SetSecurityDescriptorSddlForm($secDesc.GetSddlForm('All'))
      Set-Acl -Path $dir -AclObject $acl
    }

    # Grant NT SERVICE\sshd Full Control on logs folder (fixes OpenSSH 9.5.2.1 startup crash)
    try {
      & icacls.exe "$sshDataDir\logs" /grant "NT SERVICE\sshd:(OI)(CI)(F)" 2>$null | Out-Null
    } catch {}

    # Host keys must be owned by SYSTEM or Administrators, and no other user may have access, or sshd (running as
    # SYSTEM) rejects them and exits with "no hostkeys available". ssh-keygen -A run from a user session (e.g. an
    # unattended first-logon command) leaves that user as the owner with an explicit Modify ACE, which
    # /inheritance:r keeps, so take ownership and drop that ACE first.
    # https://github.com/PowerShell/Win32-OpenSSH/wiki/Security-protection-of-various-files-in-Win32-OpenSSH
    Get-ChildItem -Path $sshDataDir -Filter "ssh_host_*_key*" -ErrorAction SilentlyContinue | ForEach-Object {
      & icacls.exe $_.FullName /setowner "*S-1-5-18" /q | Out-Null
      & icacls.exe $_.FullName /remove:g $env:USERNAME /q | Out-Null
    }

    # Lock down private host keys: remove inheritance, grant SYSTEM and Administrators Full Control
    Get-ChildItem -Path $sshDataDir -Filter "ssh_host_*_key" -ErrorAction SilentlyContinue | ForEach-Object {
      & icacls.exe $_.FullName /inheritance:r /grant "*S-1-5-18:F" /grant "*S-1-5-32-544:F" | Out-Null
    }

    # Allow read permissions on public host keys
    Get-ChildItem -Path $sshDataDir -Filter "ssh_host_*_key.pub" -ErrorAction SilentlyContinue | ForEach-Object {
      & icacls.exe $_.FullName /inheritance:r /grant "*S-1-5-18:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-11:R" | Out-Null
    }

    # Allow read permissions on sshd_config
    if (Test-Path "$sshDataDir\sshd_config") {
      & icacls.exe "$sshDataDir\sshd_config" /inheritance:r /grant "*S-1-5-18:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-11:R" | Out-Null
    }

    # Lock down administrator authorized keys file if present
    $adminKeys = "$sshDataDir\administrators_authorized_keys"
    if (Test-Path $adminKeys) {
      & icacls.exe $adminKeys /inheritance:r /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
    }

    Write-Host "Directory and key ACL permissions repaired successfully." -ForegroundColor Green
  } catch {
    Write-Warning "Warning while configuring ACL for ${sshDataDir}: $($_.Exception.Message)"
  }
}

function Show-OpenSshStartupFailureAdvice {
  [CmdletBinding()]
  param(
    [string]$CurrentBinDir = "$env:SystemRoot\System32\OpenSSH"
  )

  Write-Host ""
  Write-Host "==========================================================================" -ForegroundColor Red
  Write-Host "[!] RECOMMENDATION: SWITCH TO OFFICIAL STANDALONE Win32-OpenSSH" -ForegroundColor Red
  Write-Host "==========================================================================" -ForegroundColor Red
  Write-Host "The native Windows Feature on Demand (FOD) OpenSSH service frequently fails" -ForegroundColor Yellow
  Write-Host "to start on Windows Server 2025/2019 due to known system package and ACL bugs." -ForegroundColor Yellow
  Write-Host ""
  Write-Host "To resolve this, uninstall the native FOD feature and install Win32-OpenSSH:" -ForegroundColor Cyan
  Write-Host "  Step 1 (Uninstall FOD):" -ForegroundColor White
  Write-Host "    .\setup-openssh-server.ps1 -Uninstall" -ForegroundColor Green
  Write-Host "  Step 2 (Install official standalone Win32-OpenSSH from GitHub):" -ForegroundColor White
  Write-Host "    .\setup-openssh-server.ps1 -UseWin32OpenSSH" -ForegroundColor Green
  Write-Host "==========================================================================" -ForegroundColor Red
  Write-Host ""
}

# Handle standalone permission repair mode (-FixPermissions)
if ($FixPermissions.IsPresent) {
  Repair-OpenSshPermissions -BinDir $openSshBinDir

  Set-Service -Name sshd -StartupType 'Automatic'
  Write-Host "Attempting to start sshd service..." -ForegroundColor Yellow
  try {
    Start-Service -Name sshd -ErrorAction Stop
    Write-Host "sshd service status: Running (Automatic)" -ForegroundColor Green
  } catch {
    Write-Warning "sshd service failed to start directly. Fetching diagnostic information..."
    $sshdExe = Join-Path $openSshBinDir "sshd.exe"
    if (Test-Path $sshdExe) {
      $diagOutput = (& cmd.exe /c "`"$sshdExe`" -t 2>&1")
      if ($diagOutput) {
        Write-Warning "sshd configuration check (sshd.exe -t) output: $diagOutput"
      }
    }
    Show-OpenSshStartupFailureAdvice -CurrentBinDir $openSshBinDir
    Write-Error "Failed to start sshd service! Follow the recommendation above to switch to Win32-OpenSSH."
  }
  return
}

# Handle uninstallation mode (-Uninstall)
if ($Uninstall.IsPresent) {
  Write-Host "=== Uninstalling OpenSSH Server ===" -ForegroundColor Cyan

  # Stop services
  if ($existingSshdService) {
    Write-Host "Stopping sshd service..." -ForegroundColor Yellow
    Stop-Service -Name sshd -Force -ErrorAction SilentlyContinue
  }
  $sshAgentService = Get-Service -Name ssh-agent -ErrorAction SilentlyContinue
  if ($sshAgentService) {
    Write-Host "Stopping ssh-agent service..." -ForegroundColor Yellow
    Stop-Service -Name ssh-agent -Force -ErrorAction SilentlyContinue
  }

  # Uninstall Windows native Feature on Demand (FOD)
  try {
    $capability = Get-WindowsCapability -Online -ErrorAction SilentlyContinue | Where-Object { $_.Name -like 'OpenSSH.Server*' }
    if ($capability -and $capability.State -eq 'Installed') {
      Write-Host "Removing Windows native OpenSSH.Server Feature on Demand (FOD)..." -ForegroundColor Yellow
      Remove-WindowsCapability -Online -Name $capability.Name | Out-Null
      Write-Host "OpenSSH.Server native feature removed successfully." -ForegroundColor Green
    } else {
      Write-Host "OpenSSH.Server native feature is not installed on this system." -ForegroundColor DarkGray
    }
  } catch {
    Write-Warning "Error removing OpenSSH.Server optional feature: $($_.Exception.Message)"
  }

  # Uninstall Win32-OpenSSH standalone distribution (if present)
  if (Test-Path "$win32InstallDir\uninstall-sshd.ps1") {
    Write-Host "Detected Win32-OpenSSH. Running uninstall-sshd.ps1 to unregister service..." -ForegroundColor Yellow
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$win32InstallDir\uninstall-sshd.ps1" | Out-Null
  }
  if (Get-Service -Name sshd -ErrorAction SilentlyContinue) {
    & sc.exe delete sshd | Out-Null
  }

  # Remove firewall rules
  $fwRules = @("OpenSSH-Server-In-TCP")
  if ($Port -ne 22) {
    $fwRules += "OpenSSH-Server-In-TCP-Custom$Port"
  }
  foreach ($rule in $fwRules) {
    if (Get-NetFirewallRule -Name $rule -ErrorAction SilentlyContinue) {
      Remove-NetFirewallRule -Name $rule -ErrorAction SilentlyContinue
      Write-Host "Removed firewall rule: $rule" -ForegroundColor Green
    }
  }

  # Handle configuration directory
  if ($PurgeData.IsPresent) {
    if (Test-Path "$env:ProgramData\ssh") {
      Write-Host "Purging directory $env:ProgramData\ssh..." -ForegroundColor Yellow
      Remove-Item -Path "$env:ProgramData\ssh" -Recurse -Force -ErrorAction SilentlyContinue
      Write-Host "Directory $env:ProgramData\ssh purged." -ForegroundColor Green
    }
  } else {
    Write-Host "Notice: Retained $env:ProgramData\ssh configuration and key files (use -PurgeData to remove)." -ForegroundColor DarkGray
  }

  Write-Host "OpenSSH Server uninstallation complete!" -ForegroundColor Green
  Write-Host "======================================" -ForegroundColor Cyan
  return
}

# Determine whether installation is needed:
# 1) Skip if -SkipInstall is specified
# 2) If a key is provided and sshd service / files already exist, auto-skip installation
$shouldInstall = $true
if ($SkipInstall.IsPresent) {
  $shouldInstall = $false
} elseif ($PublicKey.Trim() -or $KeyFilePath) {
  if ($existingSshdService -or $isWin32Installed) {
    $shouldInstall = $false
    Write-Host "[*] Detected existing sshd service. Automatically skipping installation to avoid re-triggering FOD." -ForegroundColor Cyan
  }
}

# Detect Windows Server 2019 (Build 17763)
$osBuild = [Environment]::OSVersion.Version.Build
$isServer2019 = ($osBuild -eq 17763)
$installWin32 = $UseWin32OpenSSH.IsPresent -or ($isServer2019 -and (-not $existingSshdService))

Write-Host "=== Starting OpenSSH Server Installation and Configuration ===" -ForegroundColor Cyan

# 2. Install OpenSSH server (if not skipped)
if ($shouldInstall) {
  if ($installWin32) {
    $openSshBinDir = $win32InstallDir
    Write-Host "[*] Installation mode: Official standalone Win32-OpenSSH" -ForegroundColor Yellow

    # Stop existing FOD sshd service if running
    if ($existingSshdService) {
      Stop-Service -Name sshd -Force -ErrorAction SilentlyContinue
    }

    if (-not (Test-Path "$win32InstallDir\sshd.exe")) {
      Write-Host "Downloading latest official Win32-OpenSSH release from GitHub..." -ForegroundColor Yellow
      if (-not (Test-Path $win32InstallDir)) {
        New-Item -ItemType Directory -Path $win32InstallDir -Force | Out-Null
      }

      $zipUrl = "https://github.com/PowerShell/Win32-OpenSSH/releases/latest/download/OpenSSH-Win64.zip"
      $zipPath = "$env:TEMP\OpenSSH-Win64.zip"
      $unzipTemp = "$env:TEMP\OpenSSH_unzip_temp"

      try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $zipUrl -OutFile $zipPath
        Expand-Archive -Path $zipPath -DestinationPath $unzipTemp -Force
        Copy-Item -Path "$unzipTemp\OpenSSH-Win64\*" -Destination $win32InstallDir -Recurse -Force
        Write-Host "Win32-OpenSSH deployed to: $win32InstallDir" -ForegroundColor Green
      } finally {
        Remove-Item -Path $zipPath, $unzipTemp -Recurse -Force -ErrorAction SilentlyContinue
      }
    } else {
      Write-Host "Win32-OpenSSH is already present ($win32InstallDir), skipping download." -ForegroundColor DarkGray
    }

    # Register Win32-OpenSSH service
    $installScript = "$win32InstallDir\install-sshd.ps1"
    if (Test-Path $installScript) {
      & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $installScript | Out-Null
    }

    # Add to Machine PATH environment variable if not already present
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if ($machinePath -split ';' -notcontains $win32InstallDir) {
      [Environment]::SetEnvironmentVariable('Path', "$win32InstallDir;$machinePath", 'Machine')
      $env:Path = "$win32InstallDir;$env:Path"
      Write-Host "Added $win32InstallDir to Machine PATH environment variable." -ForegroundColor Green
    }

  } else {
    Write-Host "[*] Installation mode: Windows Feature on Demand (FOD)" -ForegroundColor DarkGray
    $capability = Get-WindowsCapability -Online | Where-Object { $_.Name -like 'OpenSSH.Server*' }
    if ($capability.State -ne 'Installed') {
      Write-Host "Installing OpenSSH.Server Feature on Demand..." -ForegroundColor Yellow
      Add-WindowsCapability -Online -Name $capability.Name | Out-Null
      Write-Host "OpenSSH.Server installation completed." -ForegroundColor Green
    } else {
      Write-Host "OpenSSH.Server is already installed, skipping installation." -ForegroundColor DarkGray
    }
  }
} else {
  Write-Host "[*] Skipping server installation (already installed or explicitly skipped)." -ForegroundColor DarkGray
}

# 3. Initialize host keys and ProgramData\ssh permissions
Repair-OpenSshPermissions -BinDir $openSshBinDir

# 4. Set service to Automatic and start
Set-Service -Name sshd -StartupType 'Automatic'
if ((Get-Service -Name sshd).Status -ne 'Running') {
  Write-Host "Starting sshd service..." -ForegroundColor Yellow
  try {
    Start-Service -Name sshd -ErrorAction Stop
  } catch {
    Write-Warning "sshd service failed to start directly. Fetching diagnostic information..."
    $sshdExe = Join-Path $openSshBinDir "sshd.exe"
    if (Test-Path $sshdExe) {
      $diagOutput = (& cmd.exe /c "`"$sshdExe`" -t 2>&1")
      if ($diagOutput) {
        Write-Warning "sshd configuration check (sshd.exe -t) output: $diagOutput"
      }
    }

    # Query event viewer for recent OpenSSH errors
    try {
      $recentLog = Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'OpenSSH'; StartTime = (Get-Date).AddMinutes(-2) } -MaxEvents 3 -ErrorAction SilentlyContinue
      if ($recentLog) {
        $recentLog | ForEach-Object { Write-Warning "System Event Log [OpenSSH]: $($_.Message)" }
      }
    } catch {}

    # Retry starting service
    try {
      Start-Service -Name sshd -ErrorAction Stop
    } catch {
      Show-OpenSshStartupFailureAdvice -CurrentBinDir $openSshBinDir
      Write-Error "Failed to start sshd service! Follow the recommendation above to switch to Win32-OpenSSH, or run `"$sshdExe`" -d in an elevated console for interactive diagnostics."
    }
  }
}
Write-Host "sshd service status: Running (Automatic)" -ForegroundColor Green

# 5. Configure Windows Defender Firewall
if ($Port -eq 22) {
  $fwRule = Get-NetFirewallRule -Name "OpenSSH-Server-In-TCP" -ErrorAction SilentlyContinue
  if (-not $fwRule) {
    New-NetFirewallRule -Name "OpenSSH-Server-In-TCP" `
      -DisplayName "OpenSSH SSH Server (sshd)" `
      -Enabled True -Direction Inbound -Protocol TCP `
      -Action Allow -LocalPort 22 | Out-Null
    Write-Host "Created and enabled default firewall rule OpenSSH-Server-In-TCP (Port 22)." -ForegroundColor Green
  } elseif ($fwRule.Enabled -ne 'True') {
    Enable-NetFirewallRule -Name "OpenSSH-Server-In-TCP"
    Write-Host "Enabled default firewall rule OpenSSH-Server-In-TCP (Port 22)." -ForegroundColor Green
  } else {
    Write-Host "Firewall rule for Port 22 is already enabled." -ForegroundColor DarkGray
  }
} else {
  $customRuleName = "OpenSSH-Server-In-TCP-Custom$Port"
  $customRule = Get-NetFirewallRule -Name $customRuleName -ErrorAction SilentlyContinue
  if (-not $customRule) {
    New-NetFirewallRule -Name $customRuleName `
      -DisplayName "OpenSSH SSH Server (sshd) Port $Port" `
      -Enabled True -Direction Inbound -Protocol TCP `
      -Action Allow -LocalPort $Port | Out-Null
    Write-Host "Created and enabled custom firewall rule (Port $Port)." -ForegroundColor Green
  } elseif ($customRule.Enabled -ne 'True') {
    Enable-NetFirewallRule -Name $customRuleName
    Write-Host "Enabled custom firewall rule (Port $Port)." -ForegroundColor Green
  } else {
    Write-Host "Firewall rule for custom Port $Port is already enabled." -ForegroundColor DarkGray
  }
}

# 6. Parse public key content
$keyContent = $PublicKey.Trim()
if (-not $keyContent -and $KeyFilePath) {
  if (Test-Path $KeyFilePath) {
    $keyContent = (Get-Content -Path $KeyFilePath -Raw).Trim()
  } else {
    Write-Warning "Specified key file does not exist: $KeyFilePath"
  }
}

# 7. Configure public key authorization and ACL permissions
if ($ForUser) {
  # Standard user profile path
  $userProfileDir = "C:\Users\$ForUser"
  if (-not (Test-Path $userProfileDir)) {
    Write-Error "User profile directory not found: $userProfileDir"
  }
  $sshDir = "$userProfileDir\.ssh"
  if (-not (Test-Path $sshDir)) {
    New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
  }
  $authKeysPath = "$sshDir\authorized_keys"

  if ($keyContent) {
    Set-Content -Path $authKeysPath -Value $keyContent -Force -Encoding ascii
    Write-Host "Wrote user public key: $authKeysPath" -ForegroundColor Green
  } elseif (-not (Test-Path $authKeysPath)) {
    New-Item -ItemType File -Path $authKeysPath -Force | Out-Null
  }

  # ACL: remove inheritance, grant user and SYSTEM Full Control
  & icacls.exe $authKeysPath /inheritance:r /grant "${ForUser}:F" /grant "*S-1-5-18:F" | Out-Null
  Write-Host "Corrected ACL permissions: $authKeysPath (owner $ForUser and SYSTEM only)" -ForegroundColor Green

} else {
  # Administrator path
  $programDataSsh = "$env:ProgramData\ssh"
  if (-not (Test-Path $programDataSsh)) {
    New-Item -ItemType Directory -Path $programDataSsh -Force | Out-Null
  }
  $adminKeysPath = "$programDataSsh\administrators_authorized_keys"

  if ($keyContent) {
    Set-Content -Path $adminKeysPath -Value $keyContent -Force -Encoding ascii
    Write-Host "Wrote administrator public key: $adminKeysPath" -ForegroundColor Green
  } elseif (-not (Test-Path $adminKeysPath)) {
    New-Item -ItemType File -Path $adminKeysPath -Force | Out-Null
    Write-Host "Created empty authorized keys file (paste key content here): $adminKeysPath" -ForegroundColor Yellow
  }

  # ACL: remove inheritance, grant Administrators (*S-1-5-32-544) and SYSTEM (*S-1-5-18) Full Control
  & icacls.exe $adminKeysPath /inheritance:r /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
  Write-Host "Corrected ACL permissions: $adminKeysPath (Administrators and SYSTEM only)" -ForegroundColor Green
}

# 8. Adjust sshd_config options (if disabling password auth or using custom port)
$sshdConfigPath = "$env:ProgramData\ssh\sshd_config"
$needsRestart = $false

if (Test-Path $sshdConfigPath) {
  $configModified = $false
  $configLines = Get-Content -Path $sshdConfigPath

  if ($DisablePasswordAuth) {
    if ($configLines -notmatch '^\s*PasswordAuthentication\s+no') {
      $configLines = $configLines -replace '^\s*#?\s*PasswordAuthentication\s+.*', 'PasswordAuthentication no'
      $configModified = $true
      Write-Host "Disabled password authentication in sshd_config (PasswordAuthentication no)." -ForegroundColor Yellow
    }
  }

  if ($Port -ne 22) {
    if ($configLines -notmatch "^\s*Port\s+$Port") {
      $configLines = $configLines -replace '^\s*#?\s*Port\s+.*', "Port $Port"
      $configModified = $true
      Write-Host "Set custom port in sshd_config: $Port." -ForegroundColor Yellow
    }
  }

  if ($configModified) {
    Set-Content -Path $sshdConfigPath -Value $configLines -Encoding ascii
    $needsRestart = $true
  }
}

# 9. Restart sshd service to apply all changes
Write-Host "Restarting sshd service to apply configuration changes..." -ForegroundColor Yellow
Restart-Service -Name sshd
Write-Host "OpenSSH Server configuration complete!" -ForegroundColor Green
Write-Host "======================================" -ForegroundColor Cyan

"""##
}
