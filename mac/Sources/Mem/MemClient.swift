import Foundation

/// Talks to a long-lived `mem serve` (JSON lines over stdin/stdout). All the real work — index,
/// redaction, the AI agent, filling in secrets for copy/run — happens in the Go binary; secrets
/// never pass through this app.
final class MemClient: ObservableObject {
    typealias Event = [String: Any]

    @Published var ready = false
    @Published var problem: String?
    @Published var notesDir = ""
    @Published var model = ""

    private var proc: Process?
    private var input: FileHandle?
    private var buffer = Data()
    private var nextID = 1
    private var handlers: [Int: (Event) -> Void] = [:]
    private var restarts = 0
    private static let finalEvents: Set<String> = ["notes", "done", "error", "ok", "saved", "output"]

    func start() {
        guard let bin = MemClient.binary() else {
            problem = "mem not found. Install it: curl -fsSL https://raw.githubusercontent.com/introvert/memcli/main/install.sh | sh"
            return
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: bin)
        p.arguments = ["serve"]
        p.environment = ShellEnv.environment
        let inPipe = Pipe(), outPipe = Pipe(), errPipe = Pipe()
        p.standardInput = inPipe
        p.standardOutput = outPipe
        p.standardError = errPipe
        outPipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let data = h.availableData
            DispatchQueue.main.async { self?.consume(data) }
        }
        errPipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let text = String(decoding: h.availableData, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if !text.isEmpty { DispatchQueue.main.async { self?.problem = text } }
        }
        p.terminationHandler = { [weak self] _ in
            DispatchQueue.main.async { self?.died() }
        }
        do {
            try p.run()
        } catch {
            problem = "could not start \(bin): \(error.localizedDescription)"
            return
        }
        proc = p
        input = inPipe.fileHandleForWriting
    }

    /// Sends a request; `handler` gets every event for it until the final one.
    @discardableResult
    func send(_ op: String, _ params: [String: Any] = [:], handler: @escaping (Event) -> Void = { _ in }) -> Int {
        let id = nextID
        nextID += 1
        var req = params
        req["id"] = id
        req["op"] = op
        guard let input, let data = try? JSONSerialization.data(withJSONObject: req) else {
            handler(["ev": "error", "text": problem ?? "mem is not running"])
            return id
        }
        handlers[id] = handler
        input.write(data + Data([0x0A]))
        return id
    }

    private func consume(_ data: Data) {
        buffer.append(data)
        while let nl = buffer.firstIndex(of: 0x0A) {
            let line = buffer[buffer.startIndex..<nl]
            buffer.removeSubrange(buffer.startIndex...nl)
            guard let ev = (try? JSONSerialization.jsonObject(with: line)) as? Event else { continue }
            let id = ev["id"] as? Int ?? 0
            let kind = ev["ev"] as? String ?? ""
            if id == 0 {
                if kind == "ready" {
                    ready = true
                    problem = nil
                    restarts = 0
                    notesDir = ev["notes_dir"] as? String ?? ""
                    model = ev["model"] as? String ?? ""
                }
                continue
            }
            handlers[id]?(ev)
            if MemClient.finalEvents.contains(kind) { handlers[id] = nil }
        }
    }

    private func died() {
        ready = false
        input = nil
        proc = nil
        let pending = handlers
        handlers = [:]
        for (_, h) in pending { h(["ev": "error", "text": "mem stopped"]) }
        restarts += 1
        if restarts <= 5 { // e.g. the binary was upgraded underneath us
            DispatchQueue.main.asyncAfter(deadline: .now() + Double(restarts)) { self.start() }
        }
    }

    func stop() {
        proc?.terminationHandler = nil
        proc?.terminate()
    }

    /// The bundled binary first (always matches this app), then the usual install locations.
    static func binary() -> String? {
        var candidates: [String] = []
        candidates.append(Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/mem").path)
        let home = NSHomeDirectory()
        candidates += ["\(home)/.local/bin/mem", "/opt/homebrew/bin/mem", "/usr/local/bin/mem", "\(home)/go/bin/mem"]
        for dir in (ShellEnv.environment["PATH"] ?? "").split(separator: ":") {
            candidates.append("\(dir)/mem")
        }
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0) }
    }
}

/// Apps started from Finder/launchd get a bare environment. Commands the user runs from an answer
/// (and API keys exported in shell startup files) need the one their terminal has, so ask the
/// login shell for it once.
enum ShellEnv {
    static let environment: [String: String] = {
        var env = ProcessInfo.processInfo.environment
        let shell = env["SHELL"] ?? "/bin/zsh"
        let marker = "__MEM_ENV__"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: shell)
        p.arguments = ["-ilc", "printf '\(marker)'; env -0; printf '\(marker)'"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        if (try? p.run()) != nil {
            let deadline = Date().addingTimeInterval(4)
            while p.isRunning && Date() < deadline { usleep(20_000) }
            if p.isRunning { p.terminate() }
            let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            let parts = text.components(separatedBy: marker)
            if parts.count >= 3 {
                for pair in parts[1].split(separator: "\0") {
                    guard let eq = pair.firstIndex(of: "=") else { continue }
                    let key = String(pair[..<eq])
                    if ["PWD", "SHLVL", "_", "OLDPWD"].contains(key) { continue }
                    env[key] = String(pair[pair.index(after: eq)...])
                }
            }
        }
        let path = env["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin"
        var dirs = path.split(separator: ":").map(String.init)
        for extra in ["\(NSHomeDirectory())/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"] where !dirs.contains(extra) {
            dirs.append(extra)
        }
        env["PATH"] = dirs.joined(separator: ":")
        env.removeValue(forKey: "NO_COLOR")
        return env
    }()
}
