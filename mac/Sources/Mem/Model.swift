import AppKit
import SwiftUI

struct NoteRow: Identifiable, Equatable {
    let id: String
    let title: String
    let created: String
    let tags: [String]
    let snippet: String
    let body: String
    let path: String

    init(_ d: [String: Any]) {
        id = d["id"] as? String ?? ""
        title = d["title"] as? String ?? ""
        created = d["created"] as? String ?? ""
        tags = d["tags"] as? [String] ?? []
        snippet = d["snippet"] as? String ?? ""
        body = d["body"] as? String ?? ""
        path = d["path"] as? String ?? ""
    }

    var day: String { String(created.prefix(10)) }
}

struct ActionItem: Identifiable, Equatable {
    let n: Int
    let kind: String // value, secret, cmd
    let label: String
    var id: Int { n }
}

struct Turn: Identifiable {
    let id = UUID()
    let question: String
    var answer = ""
    var status = ""
    var error: String?
    var done = false
    var sources: [NoteRow] = []
    var items: [ActionItem] = []
}

enum Mode { case search, answer }

/// State of the quick panel. Prefixes in the field: "?" asks the AI, "+" saves a note.
final class PanelModel: ObservableObject {
    let client: MemClient
    @Published var query = "" { didSet { if query != oldValue { queryChanged() } } }
    @Published var mode: Mode = .search
    @Published var notes: [NoteRow] = []
    @Published var selected = 0
    @Published var corrected = false
    @Published var turns: [Turn] = []
    @Published var confirmRun: ActionItem?
    @Published var output: (command: String, text: String, code: Int)?
    @Published var toast: String?
    @Published var recording = false
    @Published var focusToken = 0 // bumped to refocus the field

    var hide: () -> Void = {}
    private var searchID = 0
    private var askID: Int?
    private var debounce: DispatchWorkItem?
    private var toastWork: DispatchWorkItem?

    init(client: MemClient) { self.client = client }

    var asking: Bool { askID != nil }
    var current: NoteRow? { notes.indices.contains(selected) ? notes[selected] : nil }
    var lastItems: [ActionItem] { turns.last?.done == true ? turns.last!.items : [] }

    enum Intent { case search(String), ask(String), add(String) }
    var intent: Intent {
        let t = query.trimmingCharacters(in: .whitespaces)
        if mode == .search, t.hasPrefix("?") { return .ask(String(t.dropFirst()).trimmingCharacters(in: .whitespaces)) }
        if mode == .search, t.hasPrefix("+") { return .add(String(t.dropFirst()).trimmingCharacters(in: .whitespaces)) }
        return .search(t)
    }

    // MARK: lifecycle

    func shown() {
        focusToken += 1
        if mode == .search { runSearch() }
    }

    func reset() {
        if askID != nil { client.send("cancel") }
        askID = nil
        client.send("reset")
        turns = []
        confirmRun = nil
        output = nil
        mode = .search
        query = ""
        runSearch()
    }

    // MARK: search

    private func queryChanged() {
        guard mode == .search else { return }
        debounce?.cancel()
        guard case .search = intent else { return }
        let work = DispatchWorkItem { [weak self] in self?.runSearch() }
        debounce = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.06, execute: work)
    }

    func runSearch() {
        guard case let .search(q) = intent else { return }
        let id = client.send("search", ["q": q, "limit": 40]) { [weak self] ev in
            guard let self, ev["id"] as? Int == self.searchID else { return }
            if ev["ev"] as? String == "notes" {
                let previous = self.current?.id
                self.notes = (ev["notes"] as? [[String: Any]] ?? []).map(NoteRow.init)
                self.corrected = ev["corrected"] as? Bool ?? false
                self.selected = self.notes.firstIndex { $0.id == previous && !q.isEmpty } ?? 0
            }
        }
        searchID = id
    }

    func move(_ delta: Int) {
        guard !notes.isEmpty else { return }
        selected = max(0, min(notes.count - 1, selected + delta))
    }

    // MARK: actions

    /// ↩ — open the selected note, or do what the prefix says; in an answer, ask a follow-up.
    func submit(deep: Bool = false) {
        if let item = confirmRun { run(item); return }
        switch (mode, intent) {
        case (.answer, _):
            let q = query.trimmingCharacters(in: .whitespaces)
            if !q.isEmpty { ask(q, deep: deep) }
        case let (_, .ask(q)):
            if !q.isEmpty { ask(q, deep: deep) }
        case let (_, .add(text)):
            save(text)
        case (_, .search):
            if let n = current { open(n.path) }
        }
    }

    /// ⌘↩ — ask the AI whatever is in the field.
    func askField(deep: Bool) {
        if mode == .answer { submit(deep: deep); return }
        var q = query.trimmingCharacters(in: .whitespaces)
        if q.hasPrefix("?") || q.hasPrefix("+") { q = String(q.dropFirst()).trimmingCharacters(in: .whitespaces) }
        if !q.isEmpty { ask(q, deep: deep) }
    }

    /// ⌘S — save the field as a note.
    func saveField() {
        var t = query.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("+") || t.hasPrefix("?") { t = String(t.dropFirst()) }
        save(t)
    }

    func ask(_ q: String, deep: Bool) {
        let fresh = mode == .search
        if fresh { turns = [] }
        mode = .answer
        query = ""
        confirmRun = nil
        output = nil
        turns.append(Turn(question: q, status: deep ? "thinking deeply…" : "searching notes…"))
        let idx = turns.count - 1
        var params: [String: Any] = ["q": q]
        if deep { params["deep"] = true }
        if fresh { params["new"] = true }
        askID = client.send("ask", params) { [weak self] ev in
            guard let self, self.turns.indices.contains(idx) else { return }
            switch ev["ev"] as? String {
            case "status":
                self.turns[idx].status = ev["text"] as? String ?? ""
            case "text":
                self.turns[idx].answer += ev["text"] as? String ?? ""
                self.turns[idx].status = ""
            case "done":
                self.turns[idx].answer = ev["answer"] as? String ?? self.turns[idx].answer
                self.turns[idx].sources = (ev["sources"] as? [[String: Any]] ?? []).map(NoteRow.init)
                self.turns[idx].items = (ev["items"] as? [[String: Any]] ?? []).map {
                    ActionItem(n: $0["n"] as? Int ?? 0, kind: $0["kind"] as? String ?? "", label: $0["label"] as? String ?? "")
                }
                self.turns[idx].done = true
                self.turns[idx].status = ""
                self.askID = nil
            case "error":
                self.turns[idx].error = ev["text"] as? String ?? "failed"
                self.turns[idx].done = true
                self.turns[idx].status = ""
                self.askID = nil
            default: break
            }
        }
    }

    func cancelAsk() {
        guard askID != nil else { return }
        client.send("cancel")
    }

    func save(_ text: String) {
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return }
        client.send("add", ["text": t]) { [weak self] ev in
            guard let self else { return }
            if ev["ev"] as? String == "saved" {
                self.flash("Saved \(ev["note"] as? String ?? "")")
                self.query = ""
                self.runSearch()
            } else {
                self.flash(ev["text"] as? String ?? "save failed")
            }
        }
    }

    /// ⌘1–9: copy a value or secret; commands ask for confirmation first.
    func activate(_ n: Int) {
        guard let item = lastItems.first(where: { $0.n == n }) else { return }
        if item.kind == "cmd" {
            confirmRun = item
        } else {
            copyItem(item)
        }
    }

    func copyItem(_ item: ActionItem) {
        confirmRun = nil
        client.send("item", ["n": item.n, "action": "copy"]) { [weak self] ev in
            self?.flash(ev["text"] as? String ?? "")
        }
    }

    func run(_ item: ActionItem) {
        confirmRun = nil
        output = (item.label, "running…", 0)
        client.send("item", ["n": item.n, "action": "run"]) { [weak self] ev in
            guard let self else { return }
            if ev["ev"] as? String == "output" {
                let text = (ev["text"] as? String ?? "").trimmingCharacters(in: .newlines)
                self.output = (item.label, text.isEmpty ? "(no output)" : text, ev["code"] as? Int ?? 0)
            } else {
                self.output = (item.label, ev["text"] as? String ?? "failed", -1)
            }
        }
    }

    func copyAnswer() {
        guard let a = turns.last?.answer, !a.isEmpty else { return }
        client.send("copy_text", ["text": a]) { [weak self] ev in self?.flash("Copied answer") }
    }

    func copyNote(_ n: NoteRow) {
        client.send("copy_note", ["q": n.id]) { [weak self] ev in
            self?.flash(ev["text"] as? String ?? "")
        }
    }

    /// Opens a note in Obsidian when the notes folder is a vault, otherwise in the default app.
    func open(_ path: String) {
        guard !path.isEmpty else { return }
        hide()
        let url = URL(fileURLWithPath: path)
        var dir = url.deletingLastPathComponent()
        while dir.path != "/" {
            if FileManager.default.fileExists(atPath: dir.appendingPathComponent(".obsidian").path),
               NSWorkspace.shared.urlForApplication(withBundleIdentifier: "md.obsidian") != nil,
               let enc = path.addingPercentEncoding(withAllowedCharacters: .alphanumerics),
               let link = URL(string: "obsidian://open?path=\(enc)") {
                NSWorkspace.shared.open(link)
                return
            }
            dir = dir.deletingLastPathComponent()
        }
        NSWorkspace.shared.open(url)
    }

    func openNote(id: String) {
        if let n = (turns.flatMap(\.sources) + notes).first(where: { $0.id == id }) {
            open(n.path)
        } else if !client.notesDir.isEmpty {
            open(client.notesDir + "/" + id + ".md")
        }
    }

    /// Esc: step back one level; hide when there is nothing left to back out of.
    func escape() {
        if confirmRun != nil { confirmRun = nil; return }
        if output != nil { output = nil; return }
        if asking { cancelAsk(); return }
        if mode == .answer {
            mode = .search
            query = ""
            runSearch()
            return
        }
        if !query.isEmpty { query = ""; return }
        hide()
    }

    func flash(_ text: String) {
        toast = text
        toastWork?.cancel()
        let w = DispatchWorkItem { [weak self] in self?.toast = nil }
        toastWork = w
        DispatchQueue.main.asyncAfter(deadline: .now() + 2.5, execute: w)
    }
}
