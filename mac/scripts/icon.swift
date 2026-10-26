// Draws the app icon (1024px PNG): a macOS-style rounded square with a brain glyph.
// usage: swift icon.swift out.png
import AppKit

let size: CGFloat = 1024
let img = NSImage(size: NSSize(width: size, height: size))
img.lockFocus()
let inset: CGFloat = 100 // Apple's icon grid leaves a margin around the shape
let rect = NSRect(x: inset, y: inset, width: size - 2 * inset, height: size - 2 * inset)
let shape = NSBezierPath(roundedRect: rect, xRadius: 185, yRadius: 185)

NSGraphicsContext.current?.saveGraphicsState()
let shadow = NSShadow()
shadow.shadowColor = NSColor.black.withAlphaComponent(0.35)
shadow.shadowOffset = NSSize(width: 0, height: -12)
shadow.shadowBlurRadius = 28
shadow.set()
NSColor.black.setFill()
shape.fill()
NSGraphicsContext.current?.restoreGraphicsState()

NSGradient(colors: [NSColor(srgbRed: 0.42, green: 0.27, blue: 0.95, alpha: 1),
                    NSColor(srgbRed: 0.13, green: 0.55, blue: 0.98, alpha: 1)])!
    .draw(in: shape, angle: -60)

let cfg = NSImage.SymbolConfiguration(pointSize: 470, weight: .medium)
    .applying(NSImage.SymbolConfiguration(paletteColors: [.white]))
if let sym = NSImage(systemSymbolName: "brain", accessibilityDescription: nil)?.withSymbolConfiguration(cfg) {
    let s = sym.size
    sym.draw(in: NSRect(x: (size - s.width) / 2, y: (size - s.height) / 2, width: s.width, height: s.height))
}
img.unlockFocus()

let rep = NSBitmapImageRep(data: img.tiffRepresentation!)!
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
