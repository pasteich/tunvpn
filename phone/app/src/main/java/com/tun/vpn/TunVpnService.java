package com.tun.vpn;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.VpnService;
import android.os.Build;
import android.os.ParcelFileDescriptor;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.util.ArrayList;
import java.util.List;

public class TunVpnService extends VpnService {

    public static final String ACTION_START = "com.tun.vpn.START";
    public static final String ACTION_STOP  = "com.tun.vpn.STOP";

    private static final String CHANNEL = "tunvpn";
    private static final int NOTIF_ID = 1;

    private ParcelFileDescriptor vpnPfd;
    private Process tunProc;
    private Thread worker;
    private Thread prober;
    private volatile boolean engineUp = false;
    private volatile boolean stopping = false;
    private volatile long lastUnauthMs = 0;
    private volatile long lastAuthOkMs = 0;
    private volatile boolean peerLinked = false;
    private volatile int socksPort = 8888;

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent != null ? intent.getAction() : null;
        if (ACTION_STOP.equals(action)) {
            stopEverything();
            return START_NOT_STICKY;
        }
        // START
        final Config cfg = (intent != null) ? Config.fromIntent(intent) : Config.load(this);
        TunState.setPhase(TunState.STARTING, "starting tunnel…");
        startForegroundNotif();
        if (worker != null && worker.isAlive()) {
            TunState.log("[svc] already starting/running");
            return START_STICKY;
        }
        worker = new Thread(() -> bringUp(cfg), "tun-bringup");
        worker.start();
        return START_STICKY;
    }

    private void bringUp(Config cfg) {
        stopping = false;
        lastUnauthMs = 0;
        lastAuthOkMs = 0;
        peerLinked = false;
        socksPort = cfg.port;
        try {
            TunState.log("[svc] starting tunnel binary…");
            startTunnelProcess(cfg);

            if (!waitForSocks("127.0.0.1", cfg.port, 8000)) {
                TunState.log("[svc] ERROR: SOCKS " + cfg.port + " did not come up in time");
                TunState.setPhase(TunState.ERROR, "SOCKS did not start");
                stopEverything();
                return;
            }
            TunState.log("[svc] SOCKS up on 127.0.0.1:" + cfg.port);

            // Link to the server BEFORE letting the whole device flood the tunnel.
            // (sync transport won't pair if it's swamped with connections first.)
            TunState.setPhase(TunState.CONNECTING, "linking to server…");
            TunState.log("[svc] linking to server before routing device traffic…");
            boolean routed = false;
            long linkDeadline = System.currentTimeMillis() + 20000;
            int tries = 0;
            while (System.currentTimeMillis() < linkDeadline && !stopping) {
                if (probeThroughSocks(cfg.port)) { routed = true; break; }
                if (authFailing()) {
                    TunState.setPhase(TunState.AUTH_FAIL, "relay rejected pipe/token");
                    TunState.log("[svc] auth failed — check pipe/token");
                    stopEverything();
                    return;
                }
                TunState.setPhase(TunState.CONNECTING, "linking to server… (" + (++tries) + ")");
                sleep(2500);
            }
            if (stopping) return;
            TunState.log(routed ? "[svc] tunnel linked & routing — bringing up VPN"
                    : "[svc] link not confirmed yet — bringing up VPN anyway");

            Builder b = new Builder();
            b.setSession("TunVPN");
            b.setMtu(cfg.mtu);
            b.addAddress("10.0.0.2", 32);
            b.addRoute("0.0.0.0", 0);
            try {
                b.addAddress("fd00::2", 128);
                b.addRoute("::", 0);
            } catch (Throwable t) {
                TunState.log("[svc] ipv6 route skipped: " + t.getMessage());
            }
            if (cfg.dns != null && !cfg.dns.isEmpty()) {
                b.addDnsServer(cfg.dns);
            }
            // Our own UID (tunnel WS + socks) must bypass the VPN to avoid a loop.
            try {
                b.addDisallowedApplication(getPackageName());
            } catch (Exception e) {
                TunState.log("[svc] disallow self failed: " + e.getMessage());
            }
            b.setBlocking(false);

            vpnPfd = b.establish();
            if (vpnPfd == null) {
                TunState.log("[svc] ERROR: establish() returned null (VPN not prepared?)");
                stopEverything();
                return;
            }
            int fd = vpnPfd.getFd();
            TunState.log("[svc] TUN established, fd=" + fd + ", starting tun2socks…");

            String loglevel = cfg.debug ? "debug" : "warn";
            t2smobile.T2smobile.start((long) fd, "127.0.0.1:" + cfg.port, (long) cfg.mtu,
                    loglevel, cfg.dns == null ? "" : cfg.dns, cfg.blockAAAA);
            engineUp = true;

            TunState.setRunning(true);
            if (routed) {
                TunState.setPhase(TunState.CONNECTED, "traffic verified end-to-end");
            } else {
                TunState.setPhase(TunState.CONNECTING, "verifying route through the tunnel…");
            }
            TunState.log("[svc] VPN active — monitoring connectivity…");
            startProber();
        } catch (Throwable t) {
            TunState.log("[svc] ERROR: " + t);
            TunState.setPhase(TunState.ERROR, String.valueOf(t.getMessage()));
            stopEverything();
        }
    }

    /**
     * Periodically proves the whole path works by doing an HTTP request through
     * the local SOCKS proxy (phone → tunnel client → server → internet). Only
     * then do we claim "Connected". Also reflects an auth failure seen in logs.
     */
    private void startProber() {
        prober = new Thread(() -> {
            int fails = 0;
            boolean everOk = false;
            while (engineUp && !stopping) {
                boolean ok = probeThroughSocks(socksPort);
                if (ok) {
                    fails = 0;
                    everOk = true;
                    TunState.setPhase(TunState.CONNECTED, "traffic verified end-to-end");
                    sleep(15000);
                } else if (authFailing()) {
                    TunState.setPhase(TunState.AUTH_FAIL, "relay rejected pipe/token");
                    sleep(4000);
                } else {
                    fails++;
                    if (!everOk && fails < 4) {
                        TunState.setPhase(TunState.CONNECTING, "waiting for the server… (" + fails + ")");
                    } else {
                        TunState.setPhase(TunState.NO_ROUTE, "no traffic through the tunnel");
                    }
                    sleep(4000);
                }
            }
        }, "tun-prober");
        prober.setDaemon(true);
        prober.start();
    }

    /** SOCKS5 CONNECT to a known host and read an HTTP 204; true = whole path works. */
    private boolean probeThroughSocks(int port) {
        try (Socket s = new Socket()) {
            s.connect(new InetSocketAddress("127.0.0.1", port), 1500);
            s.setSoTimeout(6000);
            java.io.OutputStream o = s.getOutputStream();
            java.io.InputStream in = s.getInputStream();
            // greeting: VER=5, 1 method, no-auth
            o.write(new byte[]{0x05, 0x01, 0x00});
            o.flush();
            byte[] r = new byte[2];
            if (in.read(r) != 2 || r[0] != 0x05 || r[1] != 0x00) return false;
            // CONNECT to connectivitycheck.gstatic.com:80 (domain atyp)
            String host = "connectivitycheck.gstatic.com";
            byte[] hb = host.getBytes("US-ASCII");
            java.io.ByteArrayOutputStream req = new java.io.ByteArrayOutputStream();
            req.write(new byte[]{0x05, 0x01, 0x00, 0x03});
            req.write(hb.length);
            req.write(hb);
            req.write((80 >> 8) & 0xff);
            req.write(80 & 0xff);
            o.write(req.toByteArray());
            o.flush();
            byte[] rep = new byte[4];
            if (in.read(rep) != 4 || rep[1] != 0x00) return false; // rep[1]=0 success
            // consume BND.ADDR + port
            int atyp = rep[3];
            int skip = (atyp == 0x01) ? 4 + 2 : (atyp == 0x04) ? 16 + 2 : (atyp == 0x03) ? (in.read() + 2) : 0;
            byte[] junk = new byte[Math.max(0, skip)];
            int got = 0;
            while (got < junk.length) { int k = in.read(junk, got, junk.length - got); if (k < 0) break; got += k; }
            // HTTP request
            String httpReq = "GET /generate_204 HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n";
            o.write(httpReq.getBytes("US-ASCII"));
            o.flush();
            byte[] buf = new byte[64];
            int n = in.read(buf);
            if (n <= 0) return false;
            String status = new String(buf, 0, n, "US-ASCII");
            return status.startsWith("HTTP/1.1 204") || status.startsWith("HTTP/1.0 204")
                    || status.contains(" 204 ") || status.startsWith("HTTP/1.1 2") || status.startsWith("HTTP/1.0 2");
        } catch (Exception e) {
            return false;
        }
    }

    private static void sleep(long ms) {
        try { Thread.sleep(ms); } catch (InterruptedException ignored) {}
    }

    private void startTunnelProcess(Config cfg) throws Exception {
        String bin = getApplicationInfo().nativeLibraryDir + "/libtun.so";
        List<String> cmd = new ArrayList<>();
        cmd.add(bin);
        cmd.add("-client");
        cmd.add("-url"); cmd.add(cfg.url);
        cmd.add("-pipe"); cmd.add(cfg.pipe);
        cmd.add("-token"); cmd.add(cfg.token);
        if (cfg.cookie != null && !cfg.cookie.isEmpty()) { cmd.add("-cookie"); cmd.add(cfg.cookie); }
        cmd.add("-transport"); cmd.add(cfg.transport);
        cmd.add("-socks"); cmd.add("127.0.0.1:" + cfg.port);
        cmd.add("-lanes"); cmd.add(String.valueOf(cfg.lanes));
        cmd.add("-sockets"); cmd.add(String.valueOf(cfg.sockets));
        cmd.add("-window"); cmd.add(String.valueOf(cfg.window));
        cmd.add("-chunk"); cmd.add(String.valueOf(cfg.chunk));
        cmd.add("-writebuf"); cmd.add(String.valueOf(cfg.writebuf));
        cmd.add("-state-cap"); cmd.add(String.valueOf(cfg.statecap));
        cmd.add("-republish"); cmd.add(cfg.republish);
        cmd.add("-cwnd"); cmd.add(String.valueOf(cfg.cwnd));
        if (cfg.base64) cmd.add("-base64");
        if (cfg.debug) cmd.add("-debug");

        TunState.log("[svc] exec: libtun.so -client -transport " + cfg.transport
                + " -socks 127.0.0.1:" + cfg.port);

        ProcessBuilder pb = new ProcessBuilder(cmd);
        pb.redirectErrorStream(true);
        pb.directory(getFilesDir());
        final Process proc = pb.start();
        tunProc = proc;

        Thread reader = new Thread(() -> {
            try (BufferedReader r = new BufferedReader(new InputStreamReader(proc.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) {
                    scanTunLine(line);          // always parse (auth/link detection)
                    if (keepLogLine(line)) TunState.log("[tun] " + line);
                }
            } catch (Exception ignored) {
            }
            try {
                int code = proc.waitFor();
                TunState.log("[tun] process exited, code=" + code);
                // If the tunnel died on its own while the VPN was up, tear down.
                if (engineUp && !stopping) {
                    stopEverything();
                }
            } catch (InterruptedException ignored) {}
        }, "tun-log");
        reader.setDaemon(true);
        reader.start();
    }

    /** Keep only meaningful lines out of the tunnel's chatty output. */
    private boolean keepLogLine(String line) {
        if (line == null) return false;
        String l = line.trim();
        if (l.isEmpty()) return false;
        // Drop the \r status HUD ("● waiting for peer  down … up … conns …")
        if (l.contains("waiting for peer") || l.contains("● linked") || l.contains("conns ")) return false;
        // Drop verbose debug frames
        if (l.contains("[dbg]") || l.contains("TX raw") || l.contains("RX unknown")) return false;
        // Drop per-connection open/close churn (hundreds under whole-device load)
        if (l.contains("[open] connID=") || l.contains("[close] connID=")) return false;
        return true;
    }

    /** Watch tunnel output for auth outcome. Note: "auth OK … reason=read-write"
     *  is SUCCESS (read-write is the permission), not an error. */
    private void scanTunLine(String line) {
        String l = line.toLowerCase();
        if (l.contains("auth ok")) {
            lastAuthOkMs = System.currentTimeMillis();
        } else if (l.contains("unauthorized") || l.contains("statuscode(4401)")) {
            lastUnauthMs = System.currentTimeMillis();
        }
        if (l.contains("linked")) peerLinked = true;
        else if (l.contains("waiting for peer")) peerLinked = false;
    }

    /** True only if the most recent auth event was a failure and it's recent. */
    private boolean authFailing() {
        return lastUnauthMs > lastAuthOkMs
                && (System.currentTimeMillis() - lastUnauthMs) < 12000;
    }

    private boolean waitForSocks(String host, int port, int timeoutMs) {
        long deadline = System.currentTimeMillis() + timeoutMs;
        while (System.currentTimeMillis() < deadline) {
            try (Socket s = new Socket()) {
                s.connect(new InetSocketAddress(host, port), 300);
                return true;
            } catch (Exception e) {
                try { Thread.sleep(200); } catch (InterruptedException ignored) { return false; }
            }
        }
        return false;
    }

    private synchronized void stopEverything() {
        stopping = true;
        if (engineUp) {
            engineUp = false;
            try { t2smobile.T2smobile.stop(); } catch (Throwable t) {
                TunState.log("[svc] engine stop: " + t);
            }
        }
        if (tunProc != null) {
            try { tunProc.destroy(); } catch (Throwable ignored) {}
            tunProc = null;
        }
        if (vpnPfd != null) {
            try { vpnPfd.close(); } catch (Throwable ignored) {}
            vpnPfd = null;
        }
        TunState.setRunning(false);
        TunState.setPhase(TunState.DISCONNECTED, "");
        TunState.log("[svc] stopped.");
        stopForeground(true);
        stopSelf();
    }

    @Override
    public void onRevoke() {
        TunState.log("[svc] VPN revoked by system.");
        stopEverything();
    }

    @Override
    public void onDestroy() {
        stopEverything();
        super.onDestroy();
    }

    private void startForegroundNotif() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel ch = new NotificationChannel(
                    CHANNEL, "TunVPN", NotificationManager.IMPORTANCE_LOW);
            ch.setDescription("VPN tunnel status");
            nm.createNotificationChannel(ch);
        }
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open,
                PendingIntent.FLAG_IMMUTABLE);

        Intent stop = new Intent(this, TunVpnService.class).setAction(ACTION_STOP);
        PendingIntent stopPi = PendingIntent.getService(this, 1, stop,
                PendingIntent.FLAG_IMMUTABLE);

        Notification n = new Notification.Builder(this, CHANNEL)
                .setContentTitle("TunVPN")
                .setContentText("Tunnel active — tap to open, or Stop")
                .setSmallIcon(R.drawable.ic_launcher_foreground)
                .setContentIntent(pi)
                .addAction(new Notification.Action.Builder(
                        null, "Stop", stopPi).build())
                .setOngoing(true)
                .build();

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIF_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTIF_ID, n);
        }
    }
}
