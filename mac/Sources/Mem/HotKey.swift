import AppKit
import Carbon.HIToolbox

/// A global shortcut through Carbon's RegisterEventHotKey: works from any app and needs no
/// Accessibility permission.
struct Shortcut: Equatable {
    var keyCode: UInt32
    var modifiers: NSEvent.ModifierFlags

    static let fallback = Shortcut(keyCode: UInt32(kVK_ANSI_M), modifiers: [.control, .option])

    static var saved: Shortcut {
        get {
            let d = UserDefaults.standard
            guard d.object(forKey: "hotkeyCode") != nil else { return fallback }
            return Shortcut(keyCode: UInt32(d.integer(forKey: "hotkeyCode")),
                            modifiers: NSEvent.ModifierFlags(rawValue: UInt(d.integer(forKey: "hotkeyMods"))))
        }
        set {
            UserDefaults.standard.set(Int(newValue.keyCode), forKey: "hotkeyCode")
            UserDefaults.standard.set(Int(newValue.modifiers.rawValue), forKey: "hotkeyMods")
        }
    }

    var carbonModifiers: UInt32 {
        var m: UInt32 = 0
        if modifiers.contains(.command) { m |= UInt32(cmdKey) }
        if modifiers.contains(.option) { m |= UInt32(optionKey) }
        if modifiers.contains(.control) { m |= UInt32(controlKey) }
        if modifiers.contains(.shift) { m |= UInt32(shiftKey) }
        return m
    }

    /// "⌃⌥M"
    var display: String {
        var s = ""
        if modifiers.contains(.control) { s += "⌃" }
        if modifiers.contains(.option) { s += "⌥" }
        if modifiers.contains(.shift) { s += "⇧" }
        if modifiers.contains(.command) { s += "⌘" }
        return s + Shortcut.keyName(keyCode)
    }

    /// For NSMenuItem.keyEquivalent (lowercase letter, or "" when it has no simple equivalent).
    var menuKey: String {
        let name = Shortcut.keyName(keyCode)
        if name == "Space" { return " " }
        return name.count == 1 ? name.lowercased() : ""
    }

    static func keyName(_ code: UInt32) -> String {
        let special: [Int: String] = [
            kVK_Space: "Space", kVK_Return: "↩", kVK_Tab: "⇥", kVK_Escape: "⎋", kVK_Delete: "⌫",
            kVK_F1: "F1", kVK_F2: "F2", kVK_F3: "F3", kVK_F4: "F4", kVK_F5: "F5", kVK_F6: "F6",
            kVK_F7: "F7", kVK_F8: "F8", kVK_F9: "F9", kVK_F10: "F10", kVK_F11: "F11", kVK_F12: "F12",
            kVK_LeftArrow: "←", kVK_RightArrow: "→", kVK_UpArrow: "↑", kVK_DownArrow: "↓",
        ]
        if let s = special[Int(code)] { return s }
        // Ask the current keyboard layout what the key types, so it is right on non-US layouts too.
        guard let source = TISCopyCurrentKeyboardLayoutInputSource()?.takeRetainedValue(),
              let ptr = TISGetInputSourceProperty(source, kTISPropertyUnicodeKeyLayoutData) else { return "?" }
        let layout = unsafeBitCast(ptr, to: CFData.self)
        var dead: UInt32 = 0
        var chars = [UniChar](repeating: 0, count: 4)
        var len = 0
        let status = CFDataGetBytePtr(layout).withMemoryRebound(to: UCKeyboardLayout.self, capacity: 1) {
            UCKeyTranslate($0, UInt16(code), UInt16(kUCKeyActionDisplay), 0, UInt32(LMGetKbdType()),
                           OptionBits(kUCKeyTranslateNoDeadKeysBit), &dead, 4, &len, &chars)
        }
        guard status == noErr, len > 0 else { return "?" }
        return String(utf16CodeUnits: chars, count: len).uppercased()
    }
}

final class HotKey {
    private var ref: EventHotKeyRef?
    private var handler: EventHandlerRef?
    private let action: () -> Void

    init(action: @escaping () -> Void) {
        self.action = action
        var spec = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        let me = Unmanaged.passUnretained(self).toOpaque()
        InstallEventHandler(GetApplicationEventTarget(), { _, _, ctx in
            guard let ctx else { return noErr }
            let hk = Unmanaged<HotKey>.fromOpaque(ctx).takeUnretainedValue()
            DispatchQueue.main.async { hk.action() }
            return noErr
        }, 1, &spec, me, &handler)
    }

    /// Returns false when the combination can't be registered (e.g. another app owns it).
    @discardableResult
    func register(_ s: Shortcut) -> Bool {
        unregister()
        let id = EventHotKeyID(signature: OSType(0x4D454D21), id: 1) // "MEM!"
        return RegisterEventHotKey(s.keyCode, s.carbonModifiers, id, GetApplicationEventTarget(), 0, &ref) == noErr
    }

    func unregister() {
        if let ref { UnregisterEventHotKey(ref) }
        ref = nil
    }
}
