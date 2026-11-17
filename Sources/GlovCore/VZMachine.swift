import Foundation
import Security
import Virtualization

/// Builds the Virtualization.framework configuration of a VZ guest from its `VMConfig`.
public enum VZConfiguration {
    /// The vsock port the Linux Guest Service listens on (docs/guest-protocol.md).
    public static let guestServicePort: UInt32 = 4022
    /// The vsock port an isolated guest connects to for the egress proxy, the only host port it can reach.
    public static let egressPort: UInt32 = 4024
    /// The Guest Service's vsock port that relays the host's connections to the guest's sshd (`glov ssh`).
    public static let sshPort: UInt32 = 4025
    /// The virtio-gpu scanout, and the size of the view `VZScreen` captures and drives it through.
    public static let displayWidth = 1280
    public static let displayHeight = 800

    /// Creates the per-VM state VZ needs and keeps across boots: the machine identifier and the EFI variable store.
    public static func prepareState(_ config: VMConfig) throws {
        let fm = FileManager.default
        try fm.createDirectory(atPath: config.directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if !fm.fileExists(atPath: config.machineIdentifierPath) {
            try VZGenericMachineIdentifier().dataRepresentation.write(to: URL(fileURLWithPath: config.machineIdentifierPath), options: .atomic)
        }
        if !fm.fileExists(atPath: config.firmwareVars) {
            _ = try VZEFIVariableStore(creatingVariableStoreAt: URL(fileURLWithPath: config.firmwareVars))
        }
    }

    /// Moves a non-empty console log to `previousConsoleLogPath`, replacing the one before: a restart that ends a hang
    /// would otherwise wipe what the hung boot wrote.
    static func keepPreviousConsoleLog(_ config: VMConfig) {
        let fm = FileManager.default
        guard let size = (try? fm.attributesOfItem(atPath: config.consoleLogPath))?[.size] as? Int, size > 0 else { return }
        try? fm.removeItem(atPath: config.previousConsoleLogPath)
        try? fm.moveItem(atPath: config.consoleLogPath, toPath: config.previousConsoleLogPath)
    }

    /// The configuration, not validated (validation needs the virtualization entitlement). Expects `prepareState` to have run.
    /// Opens a new console log, which the returned configuration keeps open; the last one becomes `previousConsoleLogPath`.
    public static func make(_ config: VMConfig) throws -> VZVirtualMachineConfiguration {
        let c = VZVirtualMachineConfiguration()
        c.cpuCount = min(max(config.cpus, VZVirtualMachineConfiguration.minimumAllowedCPUCount), VZVirtualMachineConfiguration.maximumAllowedCPUCount)
        guard let memory = config.memoryBytes else { throw GlovError.startFailed("cannot read memory size \(String(reflecting: config.memory))") }
        // VZ wants a whole number of MiB.
        c.memorySize = min(max(memory & ~UInt64((1 << 20) - 1), VZVirtualMachineConfiguration.minimumAllowedMemorySize),
            VZVirtualMachineConfiguration.maximumAllowedMemorySize)

        let platform = VZGenericPlatformConfiguration()
        let idData = try Data(contentsOf: URL(fileURLWithPath: config.machineIdentifierPath))
        guard let id = VZGenericMachineIdentifier(dataRepresentation: idData) else {
            throw GlovError.startFailed("unreadable machine identifier in \(config.machineIdentifierPath)")
        }
        platform.machineIdentifier = id
        c.platform = platform

        let boot = VZEFIBootLoader()
        boot.variableStore = VZEFIVariableStore(url: URL(fileURLWithPath: config.firmwareVars))
        c.bootLoader = boot

        var storage: [VZStorageDeviceConfiguration] = []
        if let disk = config.disk {
            let a = try VZDiskImageStorageDeviceAttachment(
                url: URL(fileURLWithPath: disk), readOnly: false, cachingMode: .automatic, synchronizationMode: .full)
            storage.append(VZVirtioBlockDeviceConfiguration(attachment: a))
        }
        for drive in config.extraDrives {
            storage.append(VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: drive.path), readOnly: true)))
        }
        c.storageDevices = storage

        if !config.isolated {
            let net = VZVirtioNetworkDeviceConfiguration()
            net.attachment = VZNATNetworkDeviceAttachment()
            if let mac = config.macAddress {
                guard let address = VZMACAddress(string: mac) else { throw GlovError.startFailed("invalid macAddress \(String(reflecting: mac))") }
                net.macAddress = address
            }
            c.networkDevices = [net]
        }

        keepPreviousConsoleLog(config)
        FileManager.default.createFile(atPath: config.consoleLogPath, contents: nil, attributes: [.posixPermissions: 0o600])
        let log = try FileHandle(forWritingTo: URL(fileURLWithPath: config.consoleLogPath))
        let console = VZVirtioConsoleDeviceSerialPortConfiguration()
        console.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: nil, fileHandleForWriting: log)
        c.serialPorts = [console]

        c.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        c.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        // An isolated guest's only way out is the egress proxy's vsock port.
        if config.guestService || config.isolated { c.socketDevices = [VZVirtioSocketDeviceConfiguration()] }

        c.directorySharingDevices = try fileSystemDevices(for: config.shares)

        let gpu = VZVirtioGraphicsDeviceConfiguration()
        gpu.scanouts = [VZVirtioGraphicsScanoutConfiguration(widthInPixels: displayWidth, heightInPixels: displayHeight)]
        c.graphicsDevices = [gpu]
        c.keyboards = [VZUSBKeyboardConfiguration()]
        c.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]
        return c
    }

    /// One virtiofs device per share, tagged `ShareMount.tag`; a `ro` share is attached read-only, so guest writes fail.
    /// The host folder must exist now: it may have been removed since `share add`.
    public static func fileSystemDevices(for shares: [VMConfig.Share]) throws -> [VZVirtioFileSystemDeviceConfiguration] {
        try shares.map { share in
            var isDirectory = ObjCBool(false)
            guard FileManager.default.fileExists(atPath: share.hostPath, isDirectory: &isDirectory), isDirectory.boolValue else {
                throw GlovError.startFailed("the host folder \(share.hostPath) of share \(share.name) is not a directory (glov share remove, or restore it)")
            }
            let tag = ShareMount.tag(for: share)
            do { try VZVirtioFileSystemDeviceConfiguration.validateTag(tag) } catch {
                throw GlovError.startFailed("share \(share.name): \((error as NSError).localizedDescription)")
            }
            let device = VZVirtioFileSystemDeviceConfiguration(tag: tag)
            device.share = VZSingleDirectoryShare(directory: VZSharedDirectory(url: URL(fileURLWithPath: share.hostPath), readOnly: share.mode == .ro))
            return device
        }
    }

    /// Whether this process carries `com.apple.security.virtualization`; without it VZ refuses every VM.
    public static var hasVirtualizationEntitlement: Bool {
        guard let task = SecTaskCreateFromSelf(nil) else { return false }
        return (SecTaskCopyValueForEntitlement(task, "com.apple.security.virtualization" as CFString, nil) as? Bool) == true
    }
}

/// A VZ guest running inside this process. Virtualization.framework requires every call on the queue the
/// `VZVirtualMachine` was created with, so that queue is this actor's executor: all isolated code runs on it.
public actor VZMachine: Machine {
    public nonisolated let config: VMConfig
    private let queue: DispatchSerialQueue
    private let activity: ActivityAssertion
    private var vm: VZVirtualMachine?
    private var delegate: StopDelegate?
    private var screen: VZScreen?
    private var holdsActivity = false
    /// Isolated VMs: takes each guest connection to `VZConfiguration.egressPort` (a blocking descriptor it owns).
    private let egress: (@Sendable (Int32) -> Void)?
    private var egressListener: GuestListener?

    public nonisolated var unownedExecutor: UnownedSerialExecutor { queue.asUnownedSerialExecutor() }

    public init(config: VMConfig, activity: ActivityAssertion = .shared, egress: (@Sendable (Int32) -> Void)? = nil) {
        self.config = config
        self.activity = activity
        self.egress = egress
        queue = DispatchSerialQueue(label: "org.glov.vz.\(config.name)")
    }

    public var isRunning: Bool {
        guard let vm else { return false }
        switch vm.state {
        case .stopped, .error: return false
        default: return true
        }
    }

    public var processID: Int32? { nil }

    public func guestIsRunning() async -> Bool { vm?.state == .running }

    /// Builds, validates and starts the VM; returns once VZ reports it started (the guest is booting). With `snapshot`
    /// (a live one, see `VZSnapshots`) it first puts the snapshot's disk and variable store back, then resumes the saved machine.
    public func start(restoring snapshot: String? = nil) async throws {
        if isRunning { throw GlovError.unsupported("VM \(config.name) is already running") }
        guard VZConfiguration.hasVirtualizationEntitlement else { throw GlovError.missingVirtualizationEntitlement }
        try VZConfiguration.prepareState(config)
        if let snapshot { try VZSnapshots.restoreFiles(config, snapshot) }
        let configuration = try VZConfiguration.make(config)
        // An invalid configuration makes VZVirtualMachine's initializer raise an Objective-C exception, which would end the daemon.
        do { try configuration.validate() } catch let e as NSError where e.domain == VZErrorDomain && e.code == VZError.Code.invalidVirtualMachineConfiguration.rawValue {
            throw GlovError.startFailed("invalid VZ configuration: \(e.localizedFailureReason ?? e.localizedDescription)")
        }
        let vm = VZVirtualMachine(configuration: configuration, queue: queue)
        // A revert restarts this actor with a new VZVirtualMachine; a late stop of the old one must not tear down the new one.
        let id = ObjectIdentifier(vm)
        let delegate = StopDelegate { [weak self] in Task { await self?.guestStopped(of: id) } }
        vm.delegate = delegate
        self.vm = vm
        self.delegate = delegate
        activity.acquire()
        holdsActivity = true
        do {
            if let snapshot {
                // The saved state needs this exact configuration, which `make` rebuilds from the same vm.json.
                let url = URL(fileURLWithPath: VZSnapshots.statePath(config, snapshot))
                try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in
                    vm.restoreMachineStateFrom(url: url) { error in if let error { c.resume(throwing: error) } else { c.resume() } }
                }
                try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in vm.resume { c.resume(with: $0) } }
            } else {
                try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in
                    vm.start { c.resume(with: $0) }
                }
            }
        } catch {
            guestStopped()
            throw GlovError.startFailed("\((error as NSError).localizedDescription) (console: \(config.consoleLogPath))")
        }
        if config.isolated, let egress, let device = vm.socketDevices.first as? VZVirtioSocketDevice {
            // The only port with a listener: VZ refuses the guest's connections to every other host port.
            let listener = GuestListener(accept: egress)
            device.setSocketListener(listener.listener, forPort: VZConfiguration.egressPort)
            egressListener = listener
        }
        // VZVirtualMachineView accepts a VM bound to another queue (macOS 27 spells this VZVirtualMachineViewAdaptor).
        let machine = UncheckedSendable(vm)
        let screen = await MainActor.run {
            VZScreen(machine: machine.value, size: CGSize(width: VZConfiguration.displayWidth, height: VZConfiguration.displayHeight))
        }
        if isRunning { self.screen = screen } else { await screen.close() }  // the guest stopped while we waited
    }

    /// A live snapshot: pauses the guest, saves its machine state, clones the disk and the variable store, resumes it.
    public func createSnapshot(_ name: String) async throws {
        guard let vm, vm.state == .running else { throw GlovError.notRunning }
        let staging = try VZSnapshots.begin(config, name: name)
        let state = staging + "/" + VZSnapshots.stateFile
        var paused = false
        do {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in vm.pause { c.resume(with: $0) } }
            paused = true
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in
                vm.saveMachineStateTo(url: URL(fileURLWithPath: state)) { error in if let error { c.resume(throwing: error) } else { c.resume() } }
            }
            let size = (try FileManager.default.attributesOfItem(atPath: state)[.size] as? NSNumber)?.intValue ?? 0
            try VZSnapshots.commit(config, name: name, staging: staging, stateSize: max(size, 1))
        } catch {
            try? FileManager.default.removeItem(atPath: staging)
            if paused { try? await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in vm.resume { c.resume(with: $0) } } }
            throw error as? SnapshotError ?? SnapshotError.failed((error as NSError).localizedDescription)
        }
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, any Error>) in vm.resume { c.resume(with: $0) } }
    }

    /// Screenshots and input; coordinates are screenshot pixels, as for QEMU.
    public func console(maxWidth: Int = 1280) throws -> Console {
        guard isRunning, let screen else { throw GlovError.notRunning }
        return Console(device: screen, maxWidth: maxWidth)
    }

    public func stop(timeout: Duration) async {
        guard let vm, isRunning else { guestStopped(); return }
        if timeout > .zero, vm.canRequestStop, (try? vm.requestStop()) != nil {
            let deadline = ContinuousClock.now + timeout
            while isRunning && ContinuousClock.now < deadline { try? await Task.sleep(for: .milliseconds(100)) }
        }
        if isRunning, vm.canStop {
            await withCheckedContinuation { (c: CheckedContinuation<Void, Never>) in vm.stop { _ in c.resume() } }
        }
        guestStopped()
    }

    /// A new connection to the Guest Service's vsock port, as a blocking file descriptor the caller owns.
    /// Fails while the VM is not running or nothing listens on the port yet.
    public func connect(port: UInt32 = VZConfiguration.guestServicePort) async throws -> Int32 {
        guard let vm, vm.state == .running, let device = vm.socketDevices.first as? VZVirtioSocketDevice else { throw GuestServiceError.notConnected }
        return try await withCheckedThrowingContinuation { (c: CheckedContinuation<Int32, any Error>) in
            device.connect(toPort: port) { result in
                switch result {
                case .success(let connection):
                    // The connection object closes its descriptor when it goes away; the channel needs one whose lifetime it controls.
                    let fd = dup(connection.fileDescriptor)
                    connection.close()
                    guard fd >= 0 else { c.resume(throwing: GuestServiceError.notConnected); return }
                    let flags = fcntl(fd, F_GETFL)
                    if flags >= 0 { _ = fcntl(fd, F_SETFL, flags & ~O_NONBLOCK) }
                    var one: Int32 = 1
                    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
                    c.resume(returning: fd)
                case .failure(let error): c.resume(throwing: error)
                }
            }
        }
    }

    private func guestStopped(of machine: ObjectIdentifier) {
        if let vm, ObjectIdentifier(vm) == machine { guestStopped() }
    }

    /// The guest powered off (or start failed, or `stop` finished): let the process nap again.
    private func guestStopped() {
        if let screen {
            self.screen = nil
            Task { @MainActor in screen.close() }
        }
        if holdsActivity {
            holdsActivity = false
            activity.release()
        }
    }
}

/// Accepts guest-initiated vsock connections and hands each over as a blocking descriptor.
private final class GuestListener: NSObject, VZVirtioSocketListenerDelegate {
    let listener = VZVirtioSocketListener()
    let accept: @Sendable (Int32) -> Void

    init(accept: @escaping @Sendable (Int32) -> Void) {
        self.accept = accept
        super.init()
        listener.delegate = self
    }

    func listener(_ listener: VZVirtioSocketListener, shouldAcceptNewConnection connection: VZVirtioSocketConnection, from socketDevice: VZVirtioSocketDevice) -> Bool {
        // As in `connect`: the connection closes its descriptor when it goes away.
        let fd = dup(connection.fileDescriptor)
        connection.close()
        guard fd >= 0 else { return false }
        let flags = fcntl(fd, F_GETFL)
        if flags >= 0 { _ = fcntl(fd, F_SETFL, flags & ~O_NONBLOCK) }
        accept(fd)
        return true
    }
}

private struct UncheckedSendable<T>: @unchecked Sendable {
    let value: T
    init(_ value: T) { self.value = value }
}

private final class StopDelegate: NSObject, VZVirtualMachineDelegate {
    let onStop: @Sendable () -> Void
    init(onStop: @escaping @Sendable () -> Void) { self.onStop = onStop }
    func guestDidStop(_ virtualMachine: VZVirtualMachine) { onStop() }
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: any Error) {
        DaemonLog.write("VZ guest stopped with an error: \(error.localizedDescription)")
        onStop()
    }
}

extension VZMachine: VsockMachine {}
