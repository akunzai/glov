import Foundation

/// The files `glov vm create` writes to a Windows VM's unattend image. They are compiled into the binaries rather than
/// shipped next to them, so the CLI finds them however it is installed or invoked (a symlink, a Homebrew prefix).
public enum WindowsAssets {
    /// The Windows editions `vm create` can install: the image name Setup selects and Microsoft's generic install key
    /// for it (installs, does not activate).
    public static let editions: [String: (imageName: String, productKey: String)] = [
        "Professional": ("Windows 11 Pro", "VK7JG-NPHTM-C97JM-9MPGT-3V66T"),
        "Core": ("Windows 11 Home", "YTMG3-N6DKC-DKB77-7M9GH-8HVX7"),
    ]

    /// `setup.ps1` writes this guest file last. Until then it may still read from the unattend drive (OpenSSH), so the
    /// daemon detaches the install drives only once the file exists.
    public static let setupDoneMarker = #"C:\glov-setup.done"#

    /// The catalog's spelling of `edition` (case-insensitive); throws when `vm create` cannot install it.
    public static func edition(_ edition: String) throws -> String {
        guard let spelled = editions.keys.first(where: { $0.caseInsensitiveCompare(edition) == .orderedSame }) else {
            throw ProvisionError("cannot install edition \(edition): use \(editions.keys.sorted().joined(separator: " or "))")
        }
        return spelled
    }

    /// `zh-tw` as Windows spells it, `zh-TW`; other shapes are left alone.
    public static func languageTag(_ tag: String) -> String {
        let parts = tag.split(separator: "-")
        guard parts.count == 2, parts[1].count == 2 else { return tag }
        return parts[0].lowercased() + "-" + parts[1].uppercased()
    }

    /// Keyboards: US first, because keys typed over QMP (`type`, `key`) would otherwise go through the language's IME
    /// (Bopomofo for zh-TW), then the language's own.
    static func inputLocale(_ language: String) -> String {
        switch language.lowercased() {
        case "en-us": "0409:00000409"
        case "zh-tw": "0409:00000409;0404:00000404"
        default: "0409:00000409;\(language)"
        }
    }

    /// The Windows time zone ID `vm create` puts in the unattend file: `requested` (a Windows ID, case-insensitive; throws
    /// when it is none) or else the one for the host's IANA zone, nil when there is none (Setup's default applies).
    public static func timeZone(requested: String?, hostIdentifier: String) throws -> String? {
        guard let requested else { return WindowsZones.windowsID(forIANA: hostIdentifier) }
        guard let spelled = WindowsZones.windowsIDs.first(where: { $0.caseInsensitiveCompare(requested) == .orderedSame }) else {
            throw ProvisionError("unknown Windows time zone \(requested) (an ID such as Taipei Standard Time; see `tzutil /l` on Windows)")
        }
        return spelled
    }

    /// `autounattend.xml` for `language` (e.g. en-US), `edition` (a key of `editions`) and the Windows ID `timeZone` (nil:
    /// Setup's default), with the account password.
    public static func autounattend(password: String, language: String, edition: String, timeZone: String? = nil) throws -> String {
        let e = editions[try Self.edition(edition)]!
        let zone = timeZone.map { "\n      <TimeZone>\(xmlEscaped($0))</TimeZone>" } ?? ""
        return autounattendTemplate
            .replacingOccurrences(of: "@LANGUAGE@", with: xmlEscaped(language))
            .replacingOccurrences(of: "@INPUT_LOCALE@", with: xmlEscaped(inputLocale(language)))
            .replacingOccurrences(of: "@IMAGE_NAME@", with: xmlEscaped(e.imageName))
            .replacingOccurrences(of: "@PRODUCT_KEY@", with: e.productKey)
            .replacingOccurrences(of: "@TIME_ZONE@", with: zone)
            .replacingOccurrences(of: "@PASSWORD@", with: xmlEscaped(password))
    }

    /// `setup.ps1`, run at first logon. With `publicKey` it also installs OpenSSH, key only, through `openSSHScript`.
    public static func setupScript(publicKey: String?) -> String {
        let ssh = publicKey.map { key in
            openSSHSection.replacingOccurrences(of: "@PUBKEY@", with: key.trimmingCharacters(in: .whitespacesAndNewlines).replacingOccurrences(of: "'", with: "''"))
        }
        return setupTemplate.replacingOccurrences(of: "@OPENSSH@\n", with: ssh ?? "").replacingOccurrences(of: "@DONE@", with: setupDoneMarker)
    }

    static func xmlEscaped(_ s: String) -> String {
        s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;").replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\"", with: "&quot;").replacingOccurrences(of: "'", with: "&apos;")
    }

    static let autounattendTemplate = #"""
        <?xml version="1.0" encoding="utf-8"?>
        <!-- Unattended Windows 11 Arm64 install, written by glov vm create. -->
        <unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <settings pass="windowsPE">
            <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <SetupUILanguage><UILanguage>@LANGUAGE@</UILanguage></SetupUILanguage>
              <InputLocale>@INPUT_LOCALE@</InputLocale>
              <SystemLocale>@LANGUAGE@</SystemLocale>
              <UILanguage>@LANGUAGE@</UILanguage>
              <UserLocale>@LANGUAGE@</UserLocale>
            </component>
            <component name="Microsoft-Windows-PnpCustomizationsWinPE" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <!-- The virtio-win drive letter is not fixed; missing paths are skipped. -->
              <DriverPaths>
                <PathAndCredentials wcm:action="add" wcm:keyValue="1"><Path>D:\NetKVM\w11\ARM64</Path></PathAndCredentials>
                <PathAndCredentials wcm:action="add" wcm:keyValue="2"><Path>E:\NetKVM\w11\ARM64</Path></PathAndCredentials>
                <PathAndCredentials wcm:action="add" wcm:keyValue="3"><Path>F:\NetKVM\w11\ARM64</Path></PathAndCredentials>
                <PathAndCredentials wcm:action="add" wcm:keyValue="4"><Path>G:\NetKVM\w11\ARM64</Path></PathAndCredentials>
              </DriverPaths>
            </component>
            <component name="Microsoft-Windows-Setup" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <RunSynchronous>
                <!-- Homebrew's ArmVirtQemu firmware has no Secure Boot; swtpm provides TPM 2.0. -->
                <RunSynchronousCommand wcm:action="add">
                  <Order>1</Order>
                  <Path>reg add HKLM\SYSTEM\Setup\LabConfig /v BypassSecureBootCheck /t REG_DWORD /d 1 /f</Path>
                </RunSynchronousCommand>
              </RunSynchronous>
              <DiskConfiguration>
                <Disk wcm:action="add">
                  <DiskID>0</DiskID>
                  <WillWipeDisk>true</WillWipeDisk>
                  <CreatePartitions>
                    <CreatePartition wcm:action="add"><Order>1</Order><Type>EFI</Type><Size>300</Size></CreatePartition>
                    <CreatePartition wcm:action="add"><Order>2</Order><Type>MSR</Type><Size>16</Size></CreatePartition>
                    <CreatePartition wcm:action="add"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
                  </CreatePartitions>
                  <ModifyPartitions>
                    <ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>FAT32</Format><Label>System</Label></ModifyPartition>
                    <ModifyPartition wcm:action="add"><Order>2</Order><PartitionID>3</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter></ModifyPartition>
                  </ModifyPartitions>
                </Disk>
              </DiskConfiguration>
              <ImageInstall>
                <OSImage>
                  <InstallFrom>
                    <MetaData wcm:action="add"><Key>/IMAGE/NAME</Key><Value>@IMAGE_NAME@</Value></MetaData>
                  </InstallFrom>
                  <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>
                </OSImage>
              </ImageInstall>
              <UserData>
                <AcceptEula>true</AcceptEula>
                <ProductKey><Key>@PRODUCT_KEY@</Key><WillShowUI>OnError</WillShowUI></ProductKey>
              </UserData>
            </component>
          </settings>
          <settings pass="specialize">
            <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <ComputerName>GLOV-WIN11</ComputerName>@TIME_ZONE@
            </component>
          </settings>
          <settings pass="oobeSystem">
            <component name="Microsoft-Windows-International-Core" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <InputLocale>@INPUT_LOCALE@</InputLocale>
              <SystemLocale>@LANGUAGE@</SystemLocale>
              <UILanguage>@LANGUAGE@</UILanguage>
              <UserLocale>@LANGUAGE@</UserLocale>
            </component>
            <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="arm64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
              <OOBE>
                <HideEULAPage>true</HideEULAPage>
                <HideLocalAccountScreen>true</HideLocalAccountScreen>
                <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
                <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
                <ProtectYourPC>3</ProtectYourPC>
              </OOBE>
              <UserAccounts>
                <LocalAccounts>
                  <LocalAccount wcm:action="add">
                    <Name>glov</Name>
                    <Group>Administrators</Group>
                    <Password><Value>@PASSWORD@</Value><PlainText>true</PlainText></Password>
                  </LocalAccount>
                </LocalAccounts>
              </UserAccounts>
              <AutoLogon>
                <Enabled>true</Enabled>
                <Username>glov</Username>
                <Password><Value>@PASSWORD@</Value><PlainText>true</PlainText></Password>
                <LogonCount>9999999</LogonCount>
              </AutoLogon>
              <FirstLogonCommands>
                <SynchronousCommand wcm:action="add">
                  <Order>1</Order>
                  <CommandLine>powershell -NoProfile -ExecutionPolicy Bypass -Command "&amp; ((Get-Volume -FileSystemLabel UNATTEND).DriveLetter + ':\setup.ps1')"</CommandLine>
                  <Description>glov setup</Description>
                </SynchronousCommand>
              </FirstLogonCommands>
            </component>
          </settings>
        </unattend>

        """#

    static let setupTemplate = #"""
        # First-logon setup, written by glov vm create: virtio drivers, power settings, the Guest Service.
        $ErrorActionPreference = 'Stop'
        Start-Transcript -Path C:\glov-setup.log -Append

        # Windows Setup keeps copies of the answer file, plain-text admin password included; AutoLogon no longer needs them.
        Remove-Item -Force -ErrorAction SilentlyContinue "$env:windir\Panther\unattend*.xml", "$env:windir\Panther\Unattend\unattend*.xml", "$env:windir\System32\Sysprep\unattend.xml"

        powercfg /hibernate off
        powercfg /change standby-timeout-ac 0
        powercfg /change monitor-timeout-ac 0
        # QEMU's RTC is UTC.
        reg add HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation /v RealTimeIsUniversal /t REG_QWORD /d 1 /f

        $virtio = (Get-Volume | Where-Object FileSystemLabel -like 'virtio-win*').DriveLetter
        if ($virtio) {
            Get-ChildItem "${virtio}:\" -Directory | ForEach-Object {
                $dir = Join-Path $_.FullName 'w11\ARM64'
                if (Test-Path $dir) { pnputil /add-driver "$dir\*.inf" /install }
            }
        }

        # Install the Guest Service first so a failure further down still leaves the VM reachable over MCP.
        # The Guest Service runs as LocalSystem, so its binary must live where only administrators can write.
        $guestServiceDir = "$env:ProgramFiles\glov"
        New-Item -ItemType Directory -Force $guestServiceDir | Out-Null
        Copy-Item "$PSScriptRoot\glov-guest.exe" $guestServiceDir -Force
        & "$guestServiceDir\glov-guest.exe" install

        # The balloon service reports the guest's memory use to the virtio-balloon device (glov vm balloon). The virtio-win
        # drive goes away after the install, so it runs from the Guest Service's directory. Optional: a failure is logged only.
        try {
            $blnsvr = "${virtio}:\Balloon\w11\ARM64\blnsvr.exe"
            if ($virtio -and (Test-Path $blnsvr)) {
                Copy-Item $blnsvr $guestServiceDir -Force
                & "$guestServiceDir\blnsvr.exe" -i
            }
        } catch { Write-Warning "balloon service: $_" }
        @OPENSSH@
        Set-Content -Path @DONE@ -Value (Get-Date -Format o)
        Stop-Transcript

        """#

    static let openSSHSection = #"""

        # OpenSSH, key only (vm create --ssh). setup-openssh-server.ps1 next to this file is an unmodified copy of
        # https://gist.github.com/akunzai/1a61e403f38948fcdbcaf5f139be0728 at revision 5518803.
        # The OpenSSH firewall rule only covers the Private profile; slirp comes up as Public.
        Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private
        # The built-in OpenSSH 9.5 (Feature on Demand) sshd crashes as a service on 26300; use Win32-OpenSSH.
        & "$PSScriptRoot\setup-openssh-server.ps1" -UseWin32OpenSSH -PublicKey '@PUBKEY@' -DisablePasswordAuth

        """#
}
