import Darwin
import Foundation

/// The Mac's memory as the kernel reports it.
public struct MacMemory: Codable, Sendable, Equatable {
    public var physical: UInt64
    /// "normal", "warning" or "critical" (`kern.memorystatus_vm_pressure_level`, what Activity Monitor's graph shows).
    public var pressure: String
    public var swapUsed: UInt64
    public var swapTotal: UInt64
    public init(physical: UInt64, pressure: String, swapUsed: UInt64, swapTotal: UInt64) {
        self.physical = physical; self.pressure = pressure; self.swapUsed = swapUsed; self.swapTotal = swapTotal
    }
}

/// What one Mac process uses, as `top` counts it (MEM, CMPRS, TIME).
public struct ProcessUsage: Sendable, Equatable {
    /// User plus system CPU time since the process started.
    public var cpuSeconds: Double
    /// Resident memory, less pages the process marked reusable.
    public var resident: UInt64
    /// Memory compressed or swapped out; nil when the kernel does not tell this process.
    public var compressed: UInt64?
}

/// Reads the Mac's memory and the resource use of the processes VMs run in, without privileges: the daemon may read
/// processes of its own user only.
public enum MacResources {
    public static func memory() -> MacMemory {
        var level: Int32 = 0
        var size = MemoryLayout<Int32>.size
        sysctlbyname("kern.memorystatus_vm_pressure_level", &level, &size, nil, 0)
        var swap = xsw_usage()
        size = MemoryLayout<xsw_usage>.size
        sysctlbyname("vm.swapusage", &swap, &size, nil, 0)
        // 1 normal, 2 warn, 4 critical (<sys/kern_memorystatus.h>, kVMPressure*).
        let pressure = level >= 4 ? "critical" : level >= 2 ? "warning" : "normal"
        return .init(physical: ProcessInfo.processInfo.physicalMemory, pressure: pressure, swapUsed: swap.xsu_used, swapTotal: swap.xsu_total)
    }

    /// CPU time and memory of `pid`; nil when it does not exist or belongs to another user.
    public static func usage(of pid: Int32) -> ProcessUsage? {
        var info = rusage_info_v4()
        let rc = withUnsafeMutablePointer(to: &info) {
            $0.withMemoryRebound(to: rusage_info_t?.self, capacity: 1) { proc_pid_rusage(pid, RUSAGE_INFO_V4, $0) }
        }
        guard rc == 0 else { return nil }
        // On Apple silicon the rusage times are in mach absolute time units, not nanoseconds.
        var timebase = mach_timebase_info_data_t()
        mach_timebase_info(&timebase)
        let ticks = Double(info.ri_user_time + info.ri_system_time)
        var usage = ProcessUsage(cpuSeconds: ticks * Double(timebase.numer) / Double(timebase.denom) / 1e9, resident: info.ri_resident_size)
        // A task name port (no task_for_pid privilege needed) allows TASK_VM_INFO, which has the compressed size top shows.
        var task: mach_port_t = 0
        if task_name_for_pid(mach_task_self_, pid, &task) == KERN_SUCCESS {
            defer { mach_port_deallocate(mach_task_self_, task) }
            var vm = task_vm_info_data_t()
            var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
            let kr = withUnsafeMutablePointer(to: &vm) {
                $0.withMemoryRebound(to: integer_t.self, capacity: Int(count)) { task_info(task, task_flavor_t(TASK_VM_INFO), $0, &count) }
            }
            if kr == KERN_SUCCESS {
                // Pages the process gave back with MADV_FREE_REUSABLE (glov-qemu does so with what a balloon takes) stay
                // resident until the kernel reclaims them, but are no longer the process's: phys_footprint leaves them out.
                usage.resident = vm.resident_size - min(vm.reusable, vm.resident_size)
                usage.compressed = vm.compressed
            }
        }
        return usage
    }

    /// The executable of the XPC service each VZ guest runs in: one process per running VM, a child of launchd.
    public static let vzServiceExecutable =
        "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"

    /// The process running `executable` that has `path` open. Virtualization.framework does not say which XPC service
    /// process runs which VM, but each holds its VM's disk image and EFI variable store open, so the open file tells.
    public static func process(holding path: String, executable: String = vzServiceExecutable) -> Int32? {
        var target = stat()
        guard stat(path, &target) == 0 else { return nil }
        let count = proc_listallpids(nil, 0)
        guard count > 0 else { return nil }
        var pids = [Int32](repeating: 0, count: Int(count) + 64)
        let n = pids.withUnsafeMutableBytes { proc_listallpids($0.baseAddress, Int32($0.count)) }
        for pid in pids.prefix(Int(max(n, 0))) where pid > 0 && self.executable(of: pid) == executable {
            if holds(pid, device: target.st_dev, inode: target.st_ino) { return pid }
        }
        return nil
    }

    private static func executable(of pid: Int32) -> String? {
        var buffer = [UInt8](repeating: 0, count: Int(MAXPATHLEN) * 4)
        let length = proc_pidpath(pid, &buffer, UInt32(buffer.count))
        guard length > 0 else { return nil }
        return String(decoding: buffer.prefix(Int(length)), as: UTF8.self)
    }

    private static func holds(_ pid: Int32, device: dev_t, inode: ino_t) -> Bool {
        let bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, nil, 0)
        guard bytes > 0 else { return false }
        var fds = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(bytes) / MemoryLayout<proc_fdinfo>.stride + 16)
        let got = fds.withUnsafeMutableBytes { proc_pidinfo(pid, PROC_PIDLISTFDS, 0, $0.baseAddress, Int32($0.count)) }
        for fd in fds.prefix(Int(max(got, 0)) / MemoryLayout<proc_fdinfo>.stride) where fd.proc_fdtype == UInt32(PROX_FDTYPE_VNODE) {
            var vnode = vnode_fdinfowithpath()
            let size = Int32(MemoryLayout<vnode_fdinfowithpath>.size)
            guard proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDVNODEPATHINFO, &vnode, size) == size else { continue }
            let st = vnode.pvip.vip_vi.vi_stat
            if st.vst_dev == UInt32(bitPattern: device) && st.vst_ino == inode { return true }
        }
        return false
    }

    /// A disk image's blocks allocated on the Mac (a raw image is sparse, a qcow2 grows) and the size the guest sees:
    /// a raw image's length, a qcow2 image's virtual size from its header.
    public static func diskImage(_ path: String) -> (allocated: UInt64, size: UInt64)? {
        var st = stat()
        guard stat(path, &st) == 0 else { return nil }
        let allocated = UInt64(st.st_blocks) * 512
        var size = UInt64(st.st_size)
        if let h = FileHandle(forReadingAtPath: path) {
            defer { try? h.close() }
            if let header = try? h.read(upToCount: 32), header.count == 32, header.prefix(4) == Data([0x51, 0x46, 0x49, 0xFB]) {
                size = header[24..<32].reduce(0) { $0 << 8 | UInt64($1) }
            }
        }
        return (allocated, size)
    }
}
