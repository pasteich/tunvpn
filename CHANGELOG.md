# Changelog

## v1.0 — Notes Mail Tunnel (first working release)

End-to-end verified on a real device (Redmi, Android 14) against a real server:
the phone routes all traffic through the tunnel and a browser on the phone shows
the **server's** public IP.

### Tunnel (Go, `main.go` etc.)
- TCP tunnel that uses a **notes.mail.ru** Yjs pipe as transport; `-client`
  exposes a local SOCKS5, `-server` dials real targets. Transports: `awareness`
  (ARQ over presence) and `sync` (append-only Yjs doc, faster).

### Android app — “Notes Mail Tunnel” (`phone/`)
- **Whole-device VPN**: `VpnService` (TUN) + in-process tun2socks (gomobile AAR)
  forwarding to the tunnel client binary's local SOCKS5. The app's own UID is
  excluded from the VPN so the tunnel's WebSocket doesn't loop back.
- **Three tabs**: Tunnel / Server / Log.
- **Deploy the server from the phone** (Server tab, over SSH via JSch): uploads
  the arch-matched binary, writes a `systemd` unit, enables & starts it, and
  verifies it's active — with a step-by-step progress bar. Plus **Update creds**
  (rewrite pipe/token + restart) and **Remove** (uninstall).
- **Honest, probe-verified status**: links to the server *before* routing device
  traffic, then confirms the path with an HTTP probe through the tunnel. Shows
  Connecting / Connected / No route / Auth failed — never a fake "connected".
- **DNS forced over the tunnel** (DNS-over-TCP to a configurable upstream, default
  `1.1.1.1`); optional **AAAA (IPv6) blocking** so apps fall back to IPv4.
- Full config with persisted settings; scrollable, de-spammed log; Start/Stop
  always reachable (bottom bar + notification action).

### Auto-login (grab pipe/token)
- New in-app **Log in & grab pipe/token** flow (`LoginActivity`): a WebView opens
  notes.mail.ru over real HTTPS; you log in normally (2FA / captcha / passkeys all
  work), and an injected WebSocket sniffer captures the pipe UUID + token and
  fills them in automatically — no DevTools, no copy-paste, no Tampermonkey.
- This is the robust equivalent of the login-relay / TLS-terminating-proxy idea
  in `auto-cookie-proxy-architecture.md`, without the content-rewriting / CSP /
  mixed-content / WebAuthn breakage that approach runs into.

### Notable fixes / decisions made along the way
- **CGO required for the Android tunnel binary** — with `CGO_ENABLED=0` the pure-Go
  resolver can't see Android's DNS servers (`lookup … on [::1]:53: connection
  refused`); built with the NDK clang instead.
- **SOCKS is TCP-only** (no UDP ASSOCIATE) → DNS is converted to DNS-over-TCP in
  the tun2socks wrapper; other UDP (QUIC) is dropped and apps fall back to TCP.
- **systemd `$` escaping** — a pipe key starts with `$`, which systemd treats as an
  env expansion; the deploy writes it as `$$` so `-pipe` isn't emptied.
- **Link before flooding** — under whole-device load the `sync` transport wouldn't
  pair if swamped with connections first; the app links, then routes.
- **`reason="read-write"` is success** — that string in the log is the auth
  permission, not an error; the status logic treats it as OK.
- **Default `sync` + `-cwnd 5500000`**, client and server args mirrored so both
  sides always agree.
- The `sync` doc is append-only and grows per session; a heavily-reused pipe can
  get slow to pair. If a client won't link, grab a fresh `-pipe`/`-token`.
