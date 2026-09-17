import SwiftUI

struct ContentView: View {
    @State private var nodes: [FleetNode] = []
    @State private var checks: [DoctorCheck] = []
    @State private var selection: FleetNode?
    @State private var err: String?
    @State private var showAgent = false
    @State private var showInit = false
    @State private var loaded = false

    var body: some View {
        NavigationSplitView {
            List(nodes, selection: $selection) { n in
                HStack {
                    Circle()
                        .fill(n.status == "ok" ? .green : n.status == "catching-up" ? .orange : .red)
                        .frame(width: 8, height: 8)
                    VStack(alignment: .leading) {
                        Text(n.name).font(.headline)
                        Text("\(n.status) · h\(n.height) · \(n.peers)p")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                .tag(n)
            }
            .navigationTitle("cometcli")
            .toolbar {
                ToolbarItem {
                    Button { Task { await refresh() } } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                }
                ToolbarItem {
                    Button { showInit = true } label: {
                        Image(systemName: "plus.circle")
                    }
                    .help("add a profile — runs cometcli init")
                }
                ToolbarItem {
                    Button { showAgent.toggle() } label: {
                        Image(systemName: "terminal")
                    }
                }
            }
        } detail: {
            if showInit {
                TermView(args: ["init"]) {
                    showInit = false
                    Task { await refresh() }
                }
                .navigationTitle("setup")
            } else if showAgent {
                TermView(args: ["agent"])
                    .navigationTitle("agent")
            } else if loaded && nodes.isEmpty && err == nil {
                welcomeView
            } else {
                VStack(alignment: .leading) {
                    if let e = err {
                        Text(e).foregroundStyle(.red).padding()
                    }
                    List(checks) { c in
                        HStack {
                            Image(systemName: c.ok ? (c.warn ? "exclamationmark.triangle" : "checkmark.circle")
                                                   : "xmark.circle")
                                .foregroundStyle(c.ok ? (c.warn ? .orange : .green) : .red)
                            Text(c.name).font(.body.bold())
                            Spacer()
                            Text(c.detail).foregroundStyle(.secondary)
                        }
                    }
                }
                .navigationTitle(selection?.name ?? "health")
            }
        }
        .task {
            await refresh()
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 10_000_000_000)
                await refresh()
            }
        }
    }

    var welcomeView: some View {
        VStack(spacing: 16) {
            Image(systemName: "antenna.radiowaves.left.and.right")
                .font(.system(size: 48)).foregroundStyle(.secondary)
            Text("no profiles configured").font(.title2)
            Text("cometcli needs at least one node profile.\nThe setup wizard probes your node's endpoints and fills in chain params automatically.")
                .multilineTextAlignment(.center).foregroundStyle(.secondary)
            Button("run setup wizard") { showInit = true }
                .buttonStyle(.borderedProminent)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .navigationTitle("welcome")
    }

    func refresh() async {
        loaded = true
        do {
            let fleet = try await CLI.run(["fleet", "status"], as: FleetStatus.self)
            nodes = fleet.nodes.sorted { $0.name < $1.name }
            if !nodes.isEmpty {
                let doc = try await CLI.run(["doctor"], as: DoctorResult.self)
                checks = doc.checks
            }
            err = nil
        } catch {
            err = error.localizedDescription
        }
    }
}
