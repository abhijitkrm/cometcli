#!/usr/bin/env bash
# Build CometCLI.app — SwiftUI shell + SwiftTerm + bundled cometcli binary.
# Produces macos/build/CometCLI.app. Unsigned; sign/notarize for distribution.
set -euo pipefail
cd "$(dirname "$0")/.."

APP="macos/build/CometCLI.app"
echo "building cometcli (release)…"
CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/abhijitkrm/cometcli/internal/cli.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
  -o /tmp/cometcli-app-bin ./cmd/cometcli

echo "building SwiftUI shell…"
(cd macos && swift build -c release 2>&1 | tail -3)

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp macos/.build/release/CometCLI "$APP/Contents/MacOS/CometCLI"
cp /tmp/cometcli-app-bin "$APP/Contents/Resources/cometcli"
chmod +x "$APP/Contents/Resources/cometcli"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>CometCLI</string>
  <key>CFBundleIdentifier</key><string>com.cometcli.app</string>
  <key>CFBundleVersion</key><string>0.1.0</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundleExecutable</key><string>CometCLI</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
</dict></plist>
PLIST

# ad-hoc sign so Gatekeeper lets it run locally
codesign --force --deep --sign - "$APP" 2>/dev/null || true
echo "✓ $APP — open it or: open $APP"
