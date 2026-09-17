import Foundation

/// Locates the cometcli binary: bundled copy first, then common PATH dirs.
enum CLI {
    static var path: String? {
        if let bundled = Bundle.main.path(forResource: "cometcli", ofType: nil),
           FileManager.default.isExecutableFile(atPath: bundled) {
            return bundled
        }
        for cand in [
            "\(NSHomeDirectory())/.local/bin/cometcli",
            "/opt/homebrew/bin/cometcli",
            "/usr/local/bin/cometcli",
        ] where FileManager.default.isExecutableFile(atPath: cand) {
            return cand
        }
        return nil
    }

    /// Run `cometcli <args> --json` and decode the result. Times out at 20s.
    static func run<T: Decodable>(_ args: [String], as type: T.Type) async throws -> T {
        guard let bin = path else {
            throw Err.noBinary
        }
        return try await withCheckedThrowingContinuation { cont in
            let p = Process()
            p.executableURL = URL(fileURLWithPath: bin)
            p.arguments = args + ["--json"]
            let out = Pipe()
            p.standardOutput = out
            p.standardError = FileHandle.nullDevice
            var env = ProcessInfo.processInfo.environment
            env["COMETCLI_KEYRING_PASSWORD"] = env["COMETCLI_KEYRING_PASSWORD"] ?? "test"
            p.environment = env
            do {
                try p.run()
            } catch {
                cont.resume(throwing: error)
                return
            }
            DispatchQueue.global().async {
                let data = out.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                guard p.terminationStatus == 0 else {
                    cont.resume(throwing: Err.exit(p.terminationStatus))
                    return
                }
                do {
                    cont.resume(returning: try JSONDecoder().decode(T.self, from: data))
                } catch {
                    cont.resume(throwing: error)
                }
            }
        }
    }

    enum Err: LocalizedError {
        case noBinary, exit(Int32)
        var errorDescription: String? {
            switch self {
            case .noBinary: "cometcli binary not found — install it or bundle it in the app"
            case .exit(let c): "cometcli exited \(c)"
            }
        }
    }
}
