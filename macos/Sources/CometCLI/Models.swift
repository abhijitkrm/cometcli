import Foundation

struct FleetNode: Decodable, Identifiable, Hashable {
    let name: String
    let role: String
    let height: Int64
    let peers: Int
    let signing: String
    let diskPct: Double
    let status: String
    var id: String { name }

    enum CodingKeys: String, CodingKey {
        case name, role, height, peers, signing, status
        case diskPct = "disk_pct"
    }
}

struct FleetStatus: Decodable {
    let count: Int
    let nodes: [FleetNode]
}

struct DoctorCheck: Decodable, Identifiable {
    let name: String
    let ok: Bool
    let warn: Bool
    let detail: String
    var id: String { name }
}

struct DoctorResult: Decodable {
    let checks: [DoctorCheck]
    let failures: Int
    let warnings: Int
}
