import SwiftUI
import SwiftTerm

/// Embeds a real terminal running a cometcli subcommand — approvals,
/// `mon watch`, and `agent` keep their native TUI behavior.
struct TermView: NSViewRepresentable {
    let args: [String]
    var onExit: (() -> Void)? = nil

    func makeNSView(context: Context) -> LocalProcessTerminalView {
        let tv = LocalProcessTerminalView(frame: .zero)
        tv.processDelegate = context.coordinator
        if let bin = CLI.path {
            tv.startProcess(executable: bin, args: args)
        } else {
            tv.feed(text: "cometcli binary not found on PATH or in bundle\r\n")
        }
        return tv
    }

    func updateNSView(_ tv: LocalProcessTerminalView, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(onExit: onExit) }

    final class Coordinator: NSObject, LocalProcessTerminalViewDelegate {
        let onExit: (() -> Void)?
        init(onExit: (() -> Void)?) { self.onExit = onExit }

        func sizeChanged(source: LocalProcessTerminalView, newCols: Int, newRows: Int) {}
        func setTerminalTitle(source: LocalProcessTerminalView, title: String) {}
        func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
        func processTerminated(source: TerminalView, exitCode: Int32?) {
            if let onExit { DispatchQueue.main.async { onExit() } }
        }
    }
}
