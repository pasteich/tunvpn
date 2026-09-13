package com.tun.vpn;

import android.os.Handler;
import android.os.Looper;

import java.util.ArrayList;
import java.util.List;

/**
 * Process-wide state bus shared between the VpnService and the UI. Both live in
 * the same process, so a static holder with a main-thread callback is enough.
 */
public final class TunState {

    // Connection phases (authoritative status, verified by an end-to-end probe).
    public static final String DISCONNECTED = "disconnected";
    public static final String STARTING     = "starting";
    public static final String CONNECTING   = "connecting";
    public static final String CONNECTED    = "connected";   // probe succeeded
    public static final String NO_ROUTE     = "noroute";     // up but traffic doesn't pass
    public static final String AUTH_FAIL    = "authfail";    // relay rejected pipe/token
    public static final String ERROR        = "error";

    public interface Listener {
        void onRunningChanged(boolean running);
        void onLog(String line);
        void onPhase(String phase, String detail);
    }

    private static final int MAX_LINES = 800;
    private static final Object LOCK = new Object();
    private static final List<String> LOG = new ArrayList<>();
    private static final Handler MAIN = new Handler(Looper.getMainLooper());

    private static volatile boolean running = false;
    private static volatile String phase = DISCONNECTED;
    private static volatile String phaseDetail = "";
    private static volatile Listener listener;

    private TunState() {}

    public static boolean isRunning() { return running; }
    public static String phase() { return phase; }
    public static String phaseDetail() { return phaseDetail; }

    public static void setListener(Listener l) { listener = l; }

    public static List<String> snapshot() {
        synchronized (LOCK) { return new ArrayList<>(LOG); }
    }

    public static void setRunning(final boolean r) {
        running = r;
        final Listener l = listener;
        if (l != null) MAIN.post(() -> l.onRunningChanged(r));
    }

    public static void setPhase(final String p, final String detail) {
        phase = p;
        phaseDetail = detail == null ? "" : detail;
        final Listener l = listener;
        if (l != null) MAIN.post(() -> l.onPhase(p, phaseDetail));
    }

    public static void log(String line) {
        if (line == null) return;
        synchronized (LOCK) {
            LOG.add(line);
            while (LOG.size() > MAX_LINES) LOG.remove(0);
        }
        final Listener l = listener;
        if (l != null) MAIN.post(() -> l.onLog(line));
    }

    public static void clear() {
        synchronized (LOCK) { LOG.clear(); }
    }
}
