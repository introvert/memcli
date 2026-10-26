import AppKit
import SwiftUI

struct PanelView: View {
    @ObservedObject var model: PanelModel
    @ObservedObject var client: MemClient
    @FocusState private var fieldFocused: Bool

    var body: some View {
        VStack(spacing: 0) {
            field
            Divider().opacity(0.5)
            content.frame(maxWidth: .infinity, maxHeight: .infinity)
            Divider().opacity(0.5)
            footer
        }
        .ignoresSafeArea() // the (hidden) title bar would otherwise push everything down
        .background(VisualEffect().ignoresSafeArea())
        .overlay(alignment: .top) { toast }
        .overlay { if model.recording { RecordingOverlay() } }
        .environment(\.openURL, OpenURLAction { url in
            if url.scheme == "mem",
               let id = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "id" })?.value {
                model.openNote(id: id)
                return .handled
            }
            return .systemAction
        })
        .onChange(of: model.focusToken) { _ in fieldFocused = true }
        .onAppear { fieldFocused = true }
    }

    // MARK: field

    private var field: some View {
        HStack(spacing: 10) {
            Image(systemName: icon)
                .font(.system(size: 18, weight: .medium))
                .foregroundStyle(.secondary)
                .frame(width: 22)
            TextField(placeholder, text: $model.query)
                .textFieldStyle(.plain)
                .font(.system(size: 21, weight: .regular))
                .focused($fieldFocused)
                .onSubmit { model.submit() }
            if model.asking {
                ProgressView().controlSize(.small)
            }
        }
        .padding(.horizontal, 16)
        .frame(height: 54)
    }

    private var icon: String {
        if model.mode == .answer { return "bubble.left.and.text.bubble.right" }
        switch model.intent {
        case .ask: return "sparkles"
        case .add: return "square.and.pencil"
        case .search: return "magnifyingglass"
        }
    }

    private var placeholder: String {
        model.mode == .answer ? "Ask a follow-up…" : "Search memory  ·  ? ask AI  ·  + save a note"
    }

    // MARK: content

    @ViewBuilder private var content: some View {
        if let p = client.problem, !client.ready {
            Message(icon: "exclamationmark.triangle", title: "mem isn't running", detail: p)
        } else if model.mode == .answer {
            AnswerView(model: model)
        } else {
            switch model.intent {
            case let .ask(q):
                Message(icon: "sparkles", title: q.isEmpty ? "Ask your notes anything" : "Ask: \(q)",
                        detail: "↩ to ask \(client.model) · ⇧⌘↩ think deeply. Secrets in notes are never sent.")
            case let .add(t):
                Message(icon: "square.and.pencil", title: t.isEmpty ? "Type a note to save" : "Save note",
                        detail: t.isEmpty ? "#hashtags become tags" : t)
            case .search:
                if model.notes.isEmpty {
                    Message(icon: "magnifyingglass", title: model.query.isEmpty ? "No notes yet" : "No matches",
                            detail: "⌘↩ ask AI instead · ⌘S save as a note")
                } else {
                    SearchView(model: model)
                }
            }
        }
    }

    // MARK: footer

    private var footer: some View {
        HStack(spacing: 14) {
            ForEach(hints, id: \.0) { key, label in
                HStack(spacing: 4) {
                    Text(key).font(.system(size: 11, weight: .semibold, design: .rounded))
                        .padding(.horizontal, 5).padding(.vertical, 1)
                        .background(RoundedRectangle(cornerRadius: 4).fill(.quaternary))
                    Text(label).font(.system(size: 11))
                }
                .foregroundStyle(.secondary)
            }
            Spacer()
            if model.corrected && model.mode == .search {
                Text("showing close matches").font(.system(size: 11)).foregroundStyle(.tertiary)
            }
        }
        .padding(.horizontal, 14)
        .frame(height: 30)
    }

    private var hints: [(String, String)] {
        if model.confirmRun != nil { return [("↩", "run"), ("⌘C", "copy"), ("esc", "cancel")] }
        if model.mode == .answer {
            var h: [(String, String)] = []
            if !model.lastItems.isEmpty { h.append(("⌘1–\(model.lastItems.count)", "copy / run")) }
            h += [("⇧⌘C", "copy answer"), ("⌘O", "open source"), ("↩", "follow-up"), ("⌘N", "new"), ("esc", model.asking ? "stop" : "back")]
            return h
        }
        switch model.intent {
        case .ask: return [("↩", "ask"), ("⇧⌘↩", "deep"), ("esc", "clear")]
        case .add: return [("↩", "save"), ("esc", "clear")]
        case .search: return [("↩", "open"), ("⌘↩", "ask AI"), ("⇧⌘C", "copy note"), ("⌘S", "save as note"), ("esc", "close")]
        }
    }

    @ViewBuilder private var toast: some View {
        if let t = model.toast, !t.isEmpty {
            Text(t)
                .font(.system(size: 12, weight: .medium))
                .padding(.horizontal, 12).padding(.vertical, 6)
                .background(Capsule().fill(Color.green.opacity(0.9)))
                .foregroundStyle(.white)
                .padding(.top, 60)
                .transition(.opacity)
        }
    }
}

// MARK: - search

struct SearchView: View {
    @ObservedObject var model: PanelModel

    var body: some View {
        HStack(spacing: 0) {
            ScrollViewReader { proxy in
                List(Array(model.notes.enumerated()), id: \.element.id) { i, n in
                    NoteRowView(note: n, selected: i == model.selected)
                        .id(n.id)
                        .contentShape(Rectangle())
                        .onTapGesture(count: 2) { model.open(n.path) }
                        .onTapGesture { model.selected = i }
                        .listRowInsets(EdgeInsets(top: 0, leading: 6, bottom: 0, trailing: 6))
                        .listRowSeparator(.hidden)
                        .listRowBackground(Color.clear)
                }
                .listStyle(.plain)
                .scrollContentBackground(.hidden)
                .onChange(of: model.selected) { _ in
                    if let id = model.current?.id { proxy.scrollTo(id) }
                }
            }
            .frame(width: 300)
            Divider().opacity(0.5)
            if let n = model.current {
                NotePreview(note: n, model: model)
            }
        }
    }
}

struct NoteRowView: View {
    let note: NoteRow
    let selected: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(cleanTitle(note.title)).font(.system(size: 13, weight: .medium)).lineLimit(1)
            HStack(spacing: 6) {
                Text(note.day)
                if !note.tags.isEmpty { Text(note.tags.map { "#" + $0 }.joined(separator: " ")).foregroundStyle(.purple.opacity(0.8)) }
                Text(note.id.contains("/") ? (note.id as NSString).deletingLastPathComponent : "").lineLimit(1)
            }
            .font(.system(size: 11)).foregroundStyle(.secondary)
        }
        .padding(.vertical, 6).padding(.horizontal, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 7).fill(selected ? Color.accentColor.opacity(0.25) : .clear))
    }
}

struct NotePreview: View {
    let note: NoteRow
    @ObservedObject var model: PanelModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text(cleanTitle(note.title)).font(.system(size: 15, weight: .semibold)).lineLimit(2)
                    Text(note.id).font(.system(size: 11, design: .monospaced)).foregroundStyle(.secondary)
                }
                Spacer()
                Button { model.copyNote(note) } label: { Image(systemName: "doc.on.doc") }
                    .buttonStyle(.borderless).help("Copy note (⇧⌘C)")
                Button { model.open(note.path) } label: { Image(systemName: "arrow.up.forward.square") }
                    .buttonStyle(.borderless).help("Open (↩)")
            }
            .padding(14)
            ScrollView {
                Text(Markup.attributed(stripFrontTitle(note.body), linkCitations: false))
                    .font(.system(size: 13))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 14).padding(.bottom, 14)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

// MARK: - answer

struct AnswerView: View {
    @ObservedObject var model: PanelModel

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    ForEach(Array(model.turns.enumerated()), id: \.element.id) { i, t in
                        TurnView(turn: t, model: model, isLast: i == model.turns.count - 1)
                    }
                    if let o = model.output { OutputView(output: o) }
                    Color.clear.frame(height: 1).id("bottom")
                }
                .padding(16)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .onChange(of: model.turns.last?.answer) { _ in proxy.scrollTo("bottom", anchor: .bottom) }
            .onChange(of: model.turns.last?.done) { _ in
                withAnimation { proxy.scrollTo("bottom", anchor: .bottom) }
            }
            .onChange(of: model.output?.text) { _ in withAnimation { proxy.scrollTo("bottom", anchor: .bottom) } }
        }
    }
}

struct TurnView: View {
    let turn: Turn
    @ObservedObject var model: PanelModel
    let isLast: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(turn.question).font(.system(size: 13, weight: .semibold)).foregroundStyle(.secondary)
            if !turn.status.isEmpty {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.mini)
                    Text(turn.status).font(.system(size: 12)).foregroundStyle(.secondary)
                }
            }
            if !turn.answer.isEmpty { AnswerText(text: turn.answer) }
            if let e = turn.error {
                Label(e, systemImage: "exclamationmark.triangle").font(.system(size: 12)).foregroundStyle(.orange)
            }
            if isLast && turn.done {
                if !turn.items.isEmpty {
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(turn.items) { ItemRow(item: $0, model: model) }
                    }
                }
                if !turn.sources.isEmpty {
                    HStack(spacing: 6) {
                        ForEach(turn.sources) { s in
                            Button { model.open(s.path) } label: {
                                Label(s.id, systemImage: "doc.text").font(.system(size: 11)).lineLimit(1)
                            }
                            .buttonStyle(.borderless)
                            .padding(.horizontal, 8).padding(.vertical, 3)
                            .background(Capsule().fill(.quaternary))
                        }
                    }
                }
            }
        }
    }
}

struct ItemRow: View {
    let item: ActionItem
    @ObservedObject var model: PanelModel

    var body: some View {
        let confirming = model.confirmRun == item
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 10) {
                Text("⌘\(item.n)").font(.system(size: 11, weight: .semibold, design: .rounded)).foregroundStyle(.secondary)
                    .frame(width: 26, alignment: .leading)
                Image(systemName: item.kind == "cmd" ? "terminal" : item.kind == "secret" ? "key.fill" : "doc.on.doc")
                    .foregroundStyle(item.kind == "secret" ? .orange : .secondary).frame(width: 16)
                Text(item.label.split(separator: "\n").first.map(String.init) ?? "")
                    .font(.system(size: 12, design: .monospaced)).lineLimit(1).truncationMode(.middle)
                Spacer()
                if item.kind == "cmd" {
                    Button("Copy") { model.copyItem(item) }.controlSize(.small)
                    Button("Run…") { model.confirmRun = item }.controlSize(.small)
                } else {
                    Button("Copy") { model.copyItem(item) }.controlSize(.small)
                }
            }
            if confirming {
                VStack(alignment: .leading, spacing: 8) {
                    Text(item.label).font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
                        .padding(8).frame(maxWidth: .infinity, alignment: .leading)
                        .background(RoundedRectangle(cornerRadius: 6).fill(Color.black.opacity(0.25)))
                    HStack {
                        Text("Run this in your shell? Secrets (•••) are filled in locally.").font(.system(size: 11)).foregroundStyle(.secondary)
                        Spacer()
                        Button("Cancel") { model.confirmRun = nil }.controlSize(.small)
                        Button("Run") { model.run(item) }.controlSize(.small).keyboardShortcut(.defaultAction)
                    }
                }
                .padding(10)
                .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.orange.opacity(0.6)))
            }
        }
        .padding(.vertical, 3).padding(.horizontal, 8)
        .background(RoundedRectangle(cornerRadius: 7).fill(.quaternary.opacity(0.5)))
    }
}

struct OutputView: View {
    let output: (command: String, text: String, code: Int)

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Image(systemName: "terminal")
                Text(output.command).lineLimit(1).truncationMode(.middle)
                Spacer()
                if output.text != "running…" {
                    Text(output.code == 0 ? "exit 0" : "exit \(output.code)")
                        .foregroundStyle(output.code == 0 ? .green : .orange)
                }
            }
            .font(.system(size: 11, design: .monospaced)).foregroundStyle(.secondary)
            Text(output.text)
                .font(.system(size: 12, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(10)
                .background(RoundedRectangle(cornerRadius: 6).fill(Color.black.opacity(0.3)))
        }
    }
}

/// Answer text: paragraphs with inline code, citations (clickable) and secret placeholders, plus
/// fenced code blocks.
struct AnswerText: View {
    let text: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(Markup.blocks(text).enumerated()), id: \.offset) { _, b in
                if b.code {
                    Text(Markup.attributed(b.text, linkCitations: false, codeBlock: true))
                        .font(.system(size: 12, design: .monospaced))
                        .textSelection(.enabled)
                        .padding(10)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .background(RoundedRectangle(cornerRadius: 6).fill(Color.black.opacity(0.25)))
                } else {
                    Text(Markup.attributed(b.text, linkCitations: true))
                        .font(.system(size: 14))
                        .lineSpacing(2)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
    }
}

enum Markup {
    struct Block { let text: String; let code: Bool }

    /// Splits on ``` fences (an unterminated fence while streaming is still shown as code).
    static func blocks(_ s: String) -> [Block] {
        var out: [Block] = []
        var inFence = false
        var cur: [Substring] = []
        func flush() {
            let t = cur.joined(separator: "\n").trimmingCharacters(in: .newlines)
            if !t.isEmpty { out.append(Block(text: t, code: inFence)) }
            cur = []
        }
        for line in s.split(separator: "\n", omittingEmptySubsequences: false) {
            if line.trimmingCharacters(in: .whitespaces).hasPrefix("```") {
                flush()
                inFence.toggle()
            } else {
                cur.append(line)
            }
        }
        flush()
        return out
    }

    private static let token = try! NSRegularExpression(pattern: "`[^`\\n]+`|\\[REDACTED:([a-z-]+)#\\d+\\]|\\[([^\\[\\]\\n]+)\\]|\\*\\*([^*\\n]+)\\*\\*")

    static func attributed(_ s: String, linkCitations: Bool, codeBlock: Bool = false) -> AttributedString {
        var out = AttributedString()
        let ns = s as NSString
        var last = 0
        for m in token.matches(in: s, range: NSRange(location: 0, length: ns.length)) {
            out += AttributedString(ns.substring(with: NSRange(location: last, length: m.range.location - last)))
            last = m.range.location + m.range.length
            let whole = ns.substring(with: m.range)
            if m.range(at: 1).location != NSNotFound { // secret placeholder
                var a = AttributedString("‹" + ns.substring(with: m.range(at: 1)) + "›")
                a.foregroundColor = .orange
                out += a
            } else if codeBlock {
                out += AttributedString(whole)
            } else if whole.hasPrefix("`") {
                var a = AttributedString(String(whole.dropFirst().dropLast()))
                a.font = .system(size: 13, design: .monospaced)
                a.foregroundColor = .cyan
                out += a
            } else if m.range(at: 3).location != NSNotFound { // **bold**
                var a = AttributedString(ns.substring(with: m.range(at: 3)))
                a.inlinePresentationIntent = .stronglyEmphasized
                out += a
            } else { // [citation] — note ids have no spaces, or contain a slash
                let inner = ns.substring(with: m.range(at: 2))
                let isCite = !inner.contains(" ") || inner.contains("/")
                var a = AttributedString(isCite ? inner : whole)
                if isCite {
                    a.foregroundColor = .secondary
                    a.font = .system(size: 11)
                    if linkCitations {
                        var c = URLComponents()
                        c.scheme = "mem"
                        c.host = "open"
                        c.queryItems = [URLQueryItem(name: "id", value: inner)]
                        a.link = c.url
                    }
                    a = AttributedString("[") + a + AttributedString("]")
                    a.foregroundColor = .secondary
                }
                out += a
            }
        }
        out += AttributedString(ns.substring(from: last))
        return out
    }
}

// MARK: - bits

struct Message: View {
    let icon: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: icon).font(.system(size: 28)).foregroundStyle(.tertiary)
            Text(title).font(.system(size: 15, weight: .semibold)).multilineTextAlignment(.center).lineLimit(3)
            Text(detail).font(.system(size: 12)).foregroundStyle(.secondary).multilineTextAlignment(.center)
                .textSelection(.enabled).lineLimit(6)
        }
        .padding(30)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

struct RecordingOverlay: View {
    var body: some View {
        ZStack {
            Rectangle().fill(.ultraThinMaterial)
            VStack(spacing: 10) {
                Image(systemName: "keyboard").font(.system(size: 30))
                Text("Press the new shortcut").font(.system(size: 16, weight: .semibold))
                Text("Use ⌘, ⌥ or ⌃ with a key · esc cancels").font(.system(size: 12)).foregroundStyle(.secondary)
            }
        }
    }
}

struct VisualEffect: NSViewRepresentable {
    func makeNSView(context: Context) -> NSVisualEffectView {
        let v = NSVisualEffectView()
        v.material = .popover
        v.blendingMode = .behindWindow
        v.state = .active
        return v
    }

    func updateNSView(_ v: NSVisualEffectView, context: Context) {}
}

/// Titles come from the first line, which may carry inline #tags; they are shown separately.
func cleanTitle(_ t: String) -> String {
    let s = t.replacingOccurrences(of: "(^|\\s)#[\\p{L}\\p{N}_][\\p{L}\\p{N}_/-]*", with: "", options: .regularExpression)
        .trimmingCharacters(in: .whitespaces)
    return s.isEmpty ? t : s
}

/// The preview shows the title in its header; drop a leading "# Title" line from the body.
func stripFrontTitle(_ body: String) -> String {
    let lines = body.split(separator: "\n", maxSplits: 1, omittingEmptySubsequences: false)
    if let first = lines.first, first.hasPrefix("# ") { return lines.count > 1 ? String(lines[1]) : "" }
    return body
}
