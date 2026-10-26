import AppKit
import Carbon.HIToolbox
import ServiceManagement
import SwiftUI

/// Menu bar app: a status item plus a Spotlight-style panel that a global shortcut toggles.
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    let client = MemClient()
    lazy var model = PanelModel(client: client)
    var panel: QuickPanel!
    var statusItem: NSStatusItem!
    var hotKey: HotKey!
    var keyMonitor: Any?
    var recent: [NoteRow] = []

    func applicationDidFinishLaunching(_ note: Notification) {
        // Reading the login shell's environment can take a moment; do it before starting mem.
        DispatchQueue.global(qos: .userInitiated).async {
            _ = ShellEnv.environment
            DispatchQueue.main.async { self.client.start() }
        }
        panel = QuickPanel(root: PanelView(model: model, client: client))
        panel.onResign = { [weak self] in self?.hide() }
        model.hide = { [weak self] in self?.hide() }

        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        if let button = statusItem.button {
            button.image = NSImage(systemSymbolName: "brain", accessibilityDescription: "Mem")
            button.image?.isTemplate = true
            button.target = self
            button.action = #selector(statusClicked)
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }

        hotKey = HotKey { [weak self] in self?.toggle() }
        if !hotKey.register(Shortcut.saved) {
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) {
                self.alert("Shortcut \(Shortcut.saved.display) is taken", "Another app uses it. Pick a different one from the menu bar icon → Change Shortcut…")
            }
        }
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] e in
            guard let self, e.window === self.panel else { return e }
            return self.handleKey(e) ? nil : e
        }
        if !UserDefaults.standard.bool(forKey: "welcomed") {
            UserDefaults.standard.set(true, forKey: "welcomed")
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.8) { self.show() }
        }
    }

    func applicationWillTerminate(_ note: Notification) { client.stop() }

    /// Opening the app again (Finder, Spotlight, Raycast, `open -a Mem`) shows the panel.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        show()
        return false
    }

    // MARK: panel

    @objc func toggle() { panel.isVisible ? hide() : show() }

    func show(prefill: String? = nil) {
        if let prefill {
            if model.mode == .answer { model.reset() }
            model.query = prefill
        }
        panel.present()
        model.shown()
    }

    func hide() {
        guard panel.isVisible else { return }
        if model.recording { finishRecording(nil) }
        panel.orderOut(nil)
    }

    // MARK: keys

    /// Keyboard handling for the panel. Returns true when the key was consumed.
    func handleKey(_ e: NSEvent) -> Bool {
        let mods = e.modifierFlags.intersection([.command, .option, .control, .shift])
        let code = Int(e.keyCode)
        if model.recording {
            if code == kVK_Escape { finishRecording(nil); return true }
            if !mods.intersection([.command, .option, .control]).isEmpty {
                finishRecording(Shortcut(keyCode: UInt32(code), modifiers: mods))
            }
            return true
        }
        let chars = e.charactersIgnoringModifiers?.lowercased() ?? ""
        switch (code, mods) {
        case (kVK_Escape, []):
            model.escape()
        case (kVK_DownArrow, []), (kVK_ANSI_N, [.control]):
            guard model.mode == .search else { return false }
            model.move(1)
        case (kVK_UpArrow, []), (kVK_ANSI_P, [.control]):
            guard model.mode == .search else { return false }
            model.move(-1)
        case (kVK_Return, []), (kVK_ANSI_KeypadEnter, []):
            model.submit()
        case (kVK_Return, [.command]):
            model.askField(deep: false)
        case (kVK_Return, [.command, .shift]):
            model.askField(deep: true)
        case (_, [.command]) where chars == "s":
            model.saveField()
        case (_, [.command]) where chars == "n":
            model.reset()
        case (_, [.command]) where chars == "w":
            hide()
        case (_, [.command]) where chars == "o":
            if model.mode == .answer, let s = model.turns.last?.sources.first { model.open(s.path) }
            else if let n = model.current { model.open(n.path) }
        case (_, [.command]) where chars == "c" && model.confirmRun != nil:
            model.copyItem(model.confirmRun!)
        case (_, [.command, .shift]) where chars == "c":
            if model.mode == .answer { model.copyAnswer() } else if let n = model.current { model.copyNote(n) }
        case (_, [.command]) where chars.count == 1 && ("1"..."9").contains(chars) && model.mode == .answer:
            model.activate(Int(chars)!)
        case (_, [.command]) where chars == ",":
            openConfig()
        default:
            return false
        }
        return true
    }

    // MARK: menu

    @objc func statusClicked() {
        if NSApp.currentEvent?.type == .rightMouseUp || NSApp.currentEvent?.modifierFlags.contains(.option) == true {
            toggle()
            return
        }
        let menu = buildMenu()
        statusItem.menu = menu
        statusItem.button?.performClick(nil)
        statusItem.menu = nil // so the next left click comes back here
    }

    func buildMenu() -> NSMenu {
        let menu = NSMenu()
        let sc = Shortcut.saved
        let open = item("Open Mem", #selector(openPanel), key: sc.menuKey)
        open.keyEquivalentModifierMask = sc.menuKey.isEmpty ? [] : sc.modifiers
        menu.addItem(open)
        menu.addItem(item("Ask AI…", #selector(openAsk)))
        menu.addItem(item("New Note…", #selector(openAdd)))
        if !recent.isEmpty {
            menu.addItem(.separator())
            let header = NSMenuItem(title: "Recent", action: nil, keyEquivalent: "")
            header.isEnabled = false
            menu.addItem(header)
            for n in recent.prefix(6) {
                let i = item(String(cleanTitle(n.title).prefix(48)), #selector(openRecent(_:)))
                i.representedObject = n.path
                i.toolTip = n.id
                menu.addItem(i)
            }
        }
        menu.addItem(.separator())
        menu.addItem(item("Change Shortcut (\(sc.display))…", #selector(startRecording)))
        let login = item("Launch at Login", #selector(toggleLogin))
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        menu.addItem(login)
        menu.addItem(item("Open Notes Folder", #selector(openFolder)))
        menu.addItem(item("Edit Config…", #selector(openConfig)))
        menu.addItem(.separator())
        if !client.ready, let p = client.problem {
            let warn = NSMenuItem(title: "⚠︎ " + String(p.prefix(60)), action: nil, keyEquivalent: "")
            warn.isEnabled = false
            menu.addItem(warn)
        }
        menu.addItem(item("Quit Mem", #selector(NSApplication.terminate(_:)), key: "q", target: NSApp))
        refreshRecent()
        return menu
    }

    func item(_ title: String, _ action: Selector, key: String = "", target: AnyObject? = nil) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: action, keyEquivalent: key)
        i.target = target ?? self
        return i
    }

    func refreshRecent() {
        client.send("search", ["q": "", "limit": 6]) { [weak self] ev in
            if ev["ev"] as? String == "notes" {
                self?.recent = (ev["notes"] as? [[String: Any]] ?? []).map(NoteRow.init)
            }
        }
    }

    @objc func openPanel() { show() }
    @objc func openAsk() { show(prefill: "? ") }
    @objc func openAdd() { show(prefill: "+ ") }
    @objc func openRecent(_ sender: NSMenuItem) { if let p = sender.representedObject as? String { model.open(p) } }
    @objc func openFolder() {
        if !client.notesDir.isEmpty { NSWorkspace.shared.open(URL(fileURLWithPath: client.notesDir)) }
    }

    @objc func openConfig() {
        let home = ShellEnv.environment["MEM_HOME"] ?? NSHomeDirectory() + "/.mem"
        let url = URL(fileURLWithPath: home + "/config.json")
        if !FileManager.default.fileExists(atPath: url.path) {
            alert("No config yet", "Run `mem config` in a terminal to create \(url.path).")
            return
        }
        hide()
        NSWorkspace.shared.open([url], withApplicationAt: URL(fileURLWithPath: "/System/Applications/TextEdit.app"),
                                configuration: NSWorkspace.OpenConfiguration())
    }

    @objc func toggleLogin() {
        do {
            if SMAppService.mainApp.status == .enabled {
                try SMAppService.mainApp.unregister()
            } else {
                try SMAppService.mainApp.register()
            }
        } catch {
            alert("Couldn't change Launch at Login", "\(error.localizedDescription)\nMove Mem.app to Applications and try again.")
        }
    }

    // MARK: shortcut recording

    @objc func startRecording() {
        hotKey.unregister() // otherwise pressing the current shortcut would just toggle the panel
        model.recording = true
        show()
    }

    func finishRecording(_ s: Shortcut?) {
        model.recording = false
        if let s {
            if hotKey.register(s) {
                Shortcut.saved = s
                model.flash("Shortcut is now \(s.display)")
                return
            }
            model.flash("\(s.display) is taken by another app")
        }
        hotKey.register(Shortcut.saved)
    }

    func alert(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        NSApp.activate(ignoringOtherApps: true)
        a.runModal()
    }
}

/// A floating, non-activating panel (like Spotlight): it takes keyboard focus without pulling the
/// app you were in to the background, and goes away when you click elsewhere.
final class QuickPanel: NSPanel {
    var onResign: () -> Void = {}

    init<V: View>(root: V) {
        super.init(contentRect: NSRect(x: 0, y: 0, width: 760, height: 480),
                   styleMask: [.nonactivatingPanel, .titled, .fullSizeContentView, .resizable],
                   backing: .buffered, defer: false)
        isFloatingPanel = true
        level = .floating
        collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .transient]
        titleVisibility = .hidden
        titlebarAppearsTransparent = true
        isMovableByWindowBackground = true
        hidesOnDeactivate = false
        isReleasedWhenClosed = false
        standardWindowButton(.closeButton)?.isHidden = true
        standardWindowButton(.miniaturizeButton)?.isHidden = true
        standardWindowButton(.zoomButton)?.isHidden = true
        minSize = NSSize(width: 560, height: 320)
        backgroundColor = .clear
        isOpaque = false
        hasShadow = true
        let host = NSHostingView(rootView: root)
        host.wantsLayer = true
        host.layer?.cornerRadius = 14
        host.layer?.masksToBounds = true
        contentView = host
        setFrameAutosaveName("MemQuickPanel")
    }

    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { true }

    override func resignKey() {
        super.resignKey()
        onResign()
    }

    /// Shows on the screen with the mouse, horizontally centered, in the upper part.
    func present() {
        let mouse = NSEvent.mouseLocation
        let screen = NSScreen.screens.first { $0.frame.contains(mouse) } ?? NSScreen.main
        if let vf = screen?.visibleFrame {
            var f = frame
            f.origin.x = vf.midX - f.width / 2
            f.origin.y = vf.minY + vf.height * 0.62 - f.height / 2
            f.origin.y = min(f.origin.y, vf.maxY - f.height - 20)
            setFrame(f, display: false)
        }
        makeKeyAndOrderFront(nil)
    }
}

@main
enum Main {
    static func main() {
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        app.setActivationPolicy(.accessory) // menu bar only, no Dock icon
        app.run()
    }
}
