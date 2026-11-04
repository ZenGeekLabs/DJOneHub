# DJOneHub for macOS

This branch adds a native macOS service for the DJI Cellular Dongle / Quectel
EG25-G. It does not require UTM for AT-mode management.

## Current scope

- Automatic discovery of DJI (`2ca3`) and Quectel (`2c7c`) USB serial ports
- Modem, SIM, operator, registration and signal status
- Receive and send SMS through the modem AT port
- Execute explicit AT commands
- Read and switch physical eUICC profiles through AT APDU transport
- Local management page at `http://127.0.0.1:7575`
- Packaged Apple Silicon release (Intel packaging is planned separately)

The cellular data interface remains managed by macOS. This allows macOS to use
the dongle as its network connection while DJOneHub uses a separate USB serial
interface for management.

## Downloaded release

The Apple Silicon ZIP contains the executable, its libusb runtime, licenses and
the `djonehub` terminal launcher. It does not require Go, Homebrew or a separately
installed libusb on the user's Mac.

From the extracted release directory:

```sh
./djonehub start
```

The terminal remains attached to the service and the management page opens
automatically. Press `Control+C` to stop it, or run `./djonehub stop` from another
terminal in the same directory. Logs are stored in
`~/Library/Logs/DJOneHub/djonehub.log`.

## Build from source

Requirements:

- macOS 13 or newer
- Go 1.26 or newer

```sh
./scripts/package-macos-arm64.sh v0.1.0-preview
```

Release outputs:

- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/`
- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip`
- `dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip.sha256`

The packaging script downloads the official libusb source archive, verifies its
SHA-256, builds it for macOS 13 or newer and bundles the resulting runtime.

## Run

Connect the modem and run:

```sh
./dist/djonehub-macos
```

If automatic discovery picks no AT port, inspect `/dev/cu.*` and pass it:

```sh
./dist/djonehub-macos -port /dev/cu.usbmodemXXXX
```

The server only listens on localhost by default. Open:

```text
http://127.0.0.1:7575
```

## Demo without hardware

To explore the management page before buying the module, run:

```sh
./dist/djonehub-macos -demo
```

Then open `http://127.0.0.1:7575`. Demo mode provides simulated modem status,
SMS messages, AT command responses and eSIM profiles. It does not access a real
SIM, send messages or switch a physical eSIM profile.

## Launch at login

```sh
./scripts/install-macos.sh
```

Logs are written to `~/Library/Logs/DJOneHub`.

## Internet link after sleep

With usbnet set to 1 (the module's CDC-ECM internet mode), macOS can lose the
dongle's network interface after the Mac sleeps: the `Baiwang` service and its
`192.168.225.x` address disappear. DJOneHub checks the interface at startup and
after every wake. If the interface is missing or has no address, it reboots the
module with `AT+CFUN=1,1` and waits for the interface to return.

- It never changes the usbnet setting and does nothing in other usbnet modes.
- It does not reboot the module while the lid is closed. A USB dark wake with
  the lid closed waits until the lid opens. Clamshell mode with an external
  display also counts as a closed lid; see manual retry below.
- It does not reboot the module while an SMS, usbnet switch, module reboot or
  eSIM request is running, and those requests wait while the module reboots.
- `192.0.0.2` is a placeholder macOS assigns before the module's DHCP reply.
  DJOneHub waits for the real address instead of rebooting.
- If the interface still does not come back, DJOneHub tries again: 3 minutes
  after a reboot, or 30 seconds after a check the module was too busy for. It
  makes at most 10 attempts per wake, then stops and reports it.

The latest result appears on the 唤醒恢复 card of the network diagnostics page.
The log explains each step, how long it waits and when it retries; recovery
lines start with `wake recovery:` and lines about the reboot it triggers start
with `module reboot:`. While that reboot is in progress, the expected SMS poll
and status errors are not logged.

When the executable runs in the foreground of a terminal, pressing Enter there
runs recovery immediately. A manual run ignores the lid state and the attempt
limit, so it also works in clamshell mode. `./djonehub start` runs the service
in the background, so this needs the executable started directly.

To turn the feature off, start the executable with `-wake-recovery=false`.

## Platform limitations

- Native QMI/MBIM control, Linux udev and network-namespace orchestration are
  excluded from this macOS entry point.
- eSIM behavior depends on the physical eUICC and modem firmware. Profile
  switching must be verified with real hardware.
- The release uses an ad-hoc signature rather than an Apple Developer ID. On
  first run, macOS may require approval in Privacy & Security.
