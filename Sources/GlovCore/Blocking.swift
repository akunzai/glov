import Foundation

/// Runs blocking work (a socket write, a wait for a child process) off Swift concurrency's cooperative pool. That pool
/// has one thread per core and never adds more, so a pool thread blocked on a peer in this process that itself waits
/// for a pool thread hangs both: on a 3-core CI runner it hung `swift test`.
public enum Blocking {
    /// Runs `work` on `queue`, or on a thread of its own when `queue` is nil, and returns its result.
    ///
    /// Pass a serial queue to keep writes to one fd in call order: a queue made with `DispatchQueue(label:)` is serial
    /// and overcommits, so it gets a new thread whenever the others are blocked. `nonisolated(nonsending)` keeps the
    /// hand-off on the caller's actor, so two calls from one actor reach `queue` in the order they were made.
    /// Never pass a global or a concurrent queue: those do not overcommit and can run out of threads too.
    public nonisolated(nonsending) static func run<T: Sendable, E: Error>(
        on queue: DispatchQueue? = nil, _ work: @escaping @Sendable () throws(E) -> T
    ) async throws(E) -> T {
        let result: Result<T, E> = await withCheckedContinuation { c in
            if let queue {
                queue.async { c.resume(returning: Result(catching: work)) }
            } else {
                Thread.detachNewThread { c.resume(returning: Result(catching: work)) }
            }
        }
        return try result.get()
    }
}
