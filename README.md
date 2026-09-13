# tunvpn

A TCP tunnel that uses **notes.mail.ru** (a Yjs collaborative-editing pipe) as its
transport, plus an **Android VPN app** that routes the **whole device** through it.

The Go program is a two-sided tunnel:

- **client** — listens on a local SOCKS5 port and forwards streams over the pipe;
- **server** — receives streams from the pipe and dials the real targets.

The Android app (`phone/`) embeds the client as a native binary, stands up a
`VpnService` (TUN), and pipes every packet on the device through the tunnel with
an in-process [tun2socks](https://github.com/xjasonlyu/tun2socks).

```
┌─────────────┐   all traffic    ┌──────────────┐   TCP + DNS-over-TCP   ┌──────────┐   WebSocket   ┌──────────────┐        ┌──────────┐
│ phone apps  │ ───────────────► │ VpnService   │ ─────────────────────► │ libtun   │ ────────────► │ notes.mail.ru│ ─────► │  server  │ ─► internet
│ (every UID) │   via TUN fd     │  + tun2socks │   SOCKS5 @ 127.0.0.1   │ (-client)│               │   (pipe)     │        │ (-server)│
└─────────────┘                  └──────────────┘                        └──────────┘               └──────────────┘        └──────────┘
```

## Repository layout

```
.
├── main.go               # tunnel: lib0 varint codec, pipe envelope, ARQ over
│                         #   Yjs awareness, SOCKS5, bridge, client/server
├── sync_transport.go     # alternative "sync" transport (append-only Yjs doc)
├── yjs.go / yjs_test.go  # minimal Yjs update encode/decode
├── go.mod / go.sum
├── cred.txt              # DevTools console snippet to grab -pipe / -token (instructions inside)
├── tampermonkey.txt      # same grabber as a Tampermonkey userscript (install once)
└── phone/                # Android VPN app (Java) — see phone/README.md
    ├── app/…             # Activity, VpnService, config, UI
    └── t2smobile/        # gomobile wrapper around tun2socks (DNS-over-TCP)
```

## Getting your pipe & token

Both sides need a matching `-pipe` and `-token`, obtained from a live
**notes.mail.ru** session. Two ways to grab them — pick one (step-by-step
instructions live in the comment header at the top of each file):

- **`tampermonkey.txt`** *(recommended, install once)* — a
  [Tampermonkey](https://www.tampermonkey.net) userscript. Create a new script,
  paste the file, save. Then just open notes.mail.ru and a note: it shows the
  token full-screen with **Copy** buttons and prints the `-pipe … -token …` line.
- **`cred.txt`** *(one-off)* — paste it into the browser **DevTools → Console** on
  notes.mail.ru, open a note, and it prints the same line. Use `wsCopy()` to copy.

Either way, put the values in the tunnel (`-pipe`/`-token`) or the app's **Pipe
key** / **Token** fields. The pair is tied to your notes session and expires — if
the tunnel starts returning `Unauthorized (4401)`, grab a fresh pair.

## The tunnel (`main.go`)

Build/run natively:

```bash
go build -o tun .

# server side (on an exit host with normal internet):
./tun -server -pipe '$<pipe-key>' -token '<token>'

# client side (opens a local SOCKS5 proxy):
./tun -client -pipe '$<pipe-key>' -token '<token>' -socks 127.0.0.1:8888
curl -x socks5h://127.0.0.1:8888 https://example.com
```

Both sides share the same `-pipe` / `-token`. Key flags:

| flag | default | meaning |
|------|---------|---------|
| `-transport` | `awareness` | `awareness` (ARQ over presence) or `sync` (append-only Yjs doc, faster) |
| `-lanes` | 1 | parallel awareness lanes (throughput ~×lanes) |
| `-sockets` | 1 | WebSocket connections to spread lanes over |
| `-window` | 4 | unacked chunks in flight per connection per lane |
| `-chunk` | 700 | max raw bytes per chunk |
| `-writebuf` | 512 | per-connection inbound buffer depth |
| `-state-cap` | 4000 | max awareness-state bytes the relay accepts |
| `-republish` | 500ms | (re)publish/retransmit interval |
| `-cwnd` | 1 MiB | sync per-connection flow-control window |
| `-base64` | off | base64/JSON payloads instead of raw binary |

**SOCKS is TCP-only** (CONNECT). There is no UDP ASSOCIATE — see the DNS note below.

## The Android app — “Notes Mail Tunnel” (`phone/`)

A whole-device VPN wrapping the client, with three tabs (**Tunnel / Server / Log**):

- **Tunnel** — runs the tunnel as `libtun.so` (a child process) and tun2socks
  in-process (gomobile AAR), so the TUN fd is handed to tun2socks directly. The
  app's own UID is excluded from the VPN (`addDisallowedApplication`) so the
  tunnel's WebSocket doesn't loop back through itself. It **links to the server
  before routing device traffic** (sync transport won't pair if it's flooded
  first), then shows an **honest, probe-verified status** (Connecting → Connected
  / No route / Auth failed) instead of claiming "connected" blindly.
- **Server** — deploy the tunnel server to a Linux box over SSH straight from the
  phone: it uploads the right binary, writes a `systemd` unit, starts it, and
  verifies it's active — with a step-by-step progress bar. Also **Update creds**
  (rewrite pipe/token + restart) and **Remove** (uninstall). Uses the same
  pipe/token/tuning as the Tunnel tab, so both sides always match.
- **Log** — full-screen, scrollable tunnel log.
- **DNS** is forced over TCP to a configurable upstream (default `1.1.1.1`)
  through the tunnel — the phone's resolver is never used. **AAAA (IPv6)** answers
  are optionally suppressed so apps fall back to IPv4 (fixes sites that hang).
- All settings persist; Start/Stop is always reachable and also in the notification.

Full details, build steps and design notes: [`phone/README.md`](phone/README.md).

> **Note on `sync` + a fresh pipe.** The `sync` transport is an append-only Yjs
> doc that grows over a session; a pipe that's been hammered by lots of testing
> can get slow to pair ("waiting for peer"). If a client won't link, grab a fresh
> `-pipe`/`-token` (see below) and redeploy — a clean pipe pairs immediately.

### Two gotchas that matter

1. **CGO is required for the Android binary.** With `CGO_ENABLED=0` the pure-Go
   resolver can't see Android's DNS servers and dies with
   `lookup … on [::1]:53: connection refused`. Build `libtun.so` with the NDK
   clang (see `build.sh`).
2. **UDP doesn't traverse the tunnel** (SOCKS is TCP-only). DNS is handled by
   converting UDP:53 to DNS-over-TCP in the tun2socks wrapper; other UDP (QUIC,
   etc.) is dropped and apps fall back to TCP. Full UDP would require adding UDP
   ASSOCIATE to the server side.

## Building everything

`build.sh` builds the tunnel binary for Android (CGO/NDK), the tun2socks gomobile
AAR, and the debug APK. It expects a local toolchain:

- JDK 17, Android SDK (platform-34, build-tools 34), NDK r26d, Gradle 8.7
- Go 1.23+ and `gomobile`

```bash
export ANDROID_HOME=/path/to/Android/Sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/26.3.11579264
./build.sh          # → phone/app/build/outputs/apk/debug/app-debug.apk
```

See the script header and `phone/README.md` for the individual commands.

## Status

Tested on a Redmi phone, Android 14 (arm64-v8a): whole-device routing, DNS-over-TCP,
and Start/Stop verified end-to-end. Only `arm64-v8a` is built; other ABIs need the
binary and AAR rebuilt for them.
