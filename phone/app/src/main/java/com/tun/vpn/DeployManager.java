package com.tun.vpn;

import android.content.Context;
import android.content.res.AssetManager;

import com.jcraft.jsch.ChannelExec;
import com.jcraft.jsch.JSch;
import com.jcraft.jsch.Session;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.Properties;

/**
 * Deploys the tunnel server to a Linux box over SSH: uploads the right binary,
 * writes a systemd unit, and starts it. Progress is reported step-by-step.
 *
 * Mirrors exactly the arguments the client uses (from the same Config), so the
 * two sides always agree on transport/tuning. The pipe/token are '$'-escaped as
 * '$$' because systemd otherwise treats '$' as an environment expansion.
 */
public class DeployManager {

    public interface Cb {
        /** state: "run" | "ok" | "fail" */
        void step(int idx, int total, String name, String state, String detail);
        void done(boolean ok, String summary);
    }

    private static final String REMOTE_BIN = "/usr/local/bin/notes-mail-tunnel";
    private static final String UNIT_PATH = "/etc/systemd/system/notes-mail-tunnel.service";
    private static final String SERVICE = "notes-mail-tunnel";

    private static final String[] STEP_NAMES = {
            "Connect over SSH",
            "Detect server architecture",
            "Upload tunnel binary",
            "Write systemd service",
            "Enable & start service",
            "Verify service is running",
    };

    private final Context ctx;
    private final Config cfg;
    private final Cb cb;

    public DeployManager(Context ctx, Config cfg, Cb cb) {
        this.ctx = ctx.getApplicationContext();
        this.cfg = cfg;
        this.cb = cb;
    }

    public void start() {
        new Thread(this::run, "deploy").start();
    }

    /** Rewrite the unit with the current pipe/token/tuning and restart — no re-upload. */
    public void updateCreds() {
        new Thread(this::runUpdate, "deploy-update").start();
    }

    /** Stop, disable and remove the service + binary from the server. */
    public void uninstall() {
        new Thread(this::runUninstall, "deploy-uninstall").start();
    }

    private static final String[] UPDATE_STEPS = {
            "Connect over SSH", "Write updated credentials", "Restart service", "Verify service is running",
    };
    private static final String[] UNINSTALL_STEPS = {
            "Connect over SSH", "Stop & disable service", "Remove binary & unit",
    };

    private void runUpdate() {
        int total = UPDATE_STEPS.length;
        Session session = null;
        try {
            cb.step(0, total, UPDATE_STEPS[0], "run", cfg.sshUser + "@" + cfg.sshHost + ":" + cfg.sshPort);
            session = connect();
            cb.step(0, total, UPDATE_STEPS[0], "ok", "connected");

            cb.step(1, total, UPDATE_STEPS[1], "run", null);
            Exec ex = exec(session, "test -f " + REMOTE_BIN + " && echo HAVE || echo MISSING", null);
            if (ex.out.contains("MISSING")) { fail(1, total, UPDATE_STEPS[1], "server not deployed yet — use Deploy first"); return; }
            Exec un = exec(session, "cat > " + UNIT_PATH + " && echo OK", buildUnit().getBytes("UTF-8"));
            if (un.code != 0 || !un.out.contains("OK")) { fail(1, total, UPDATE_STEPS[1], trim(un)); return; }
            cb.step(1, total, UPDATE_STEPS[1], "ok", "unit rewritten");

            cb.step(2, total, UPDATE_STEPS[2], "run", "daemon-reload + restart");
            Exec r = exec(session, "systemctl daemon-reload && systemctl restart " + SERVICE + " && echo OK", null);
            if (r.code != 0) { fail(2, total, UPDATE_STEPS[2], trim(r)); return; }
            cb.step(2, total, UPDATE_STEPS[2], "ok", "restarted");

            cb.step(3, total, UPDATE_STEPS[3], "run", "checking status + logs");
            sleep(3500);
            String active = exec(session, "systemctl is-active " + SERVICE, null).out.trim();
            String logs = exec(session, "journalctl -u " + SERVICE + " --no-pager -n 12 2>&1", null).out;
            boolean authed = logs.contains("auth OK") || logs.contains("server ready");
            if ("active".equals(active) && authed) {
                cb.step(3, total, UPDATE_STEPS[3], "ok", "active · authenticated");
                cb.done(true, "Credentials updated: active, auth OK.");
            } else if ("active".equals(active)) {
                cb.step(3, total, UPDATE_STEPS[3], "ok", "active");
                cb.done(true, "Updated. Recent log:\n" + tail(logs));
            } else {
                cb.step(3, total, UPDATE_STEPS[3], "fail", "is-active=" + active);
                cb.done(false, "Not active after update. Log:\n" + tail(logs));
            }
        } catch (Throwable t) {
            cb.done(false, "Error: " + t.getMessage());
        } finally {
            if (session != null) session.disconnect();
        }
    }

    private void runUninstall() {
        int total = UNINSTALL_STEPS.length;
        Session session = null;
        try {
            cb.step(0, total, UNINSTALL_STEPS[0], "run", cfg.sshUser + "@" + cfg.sshHost + ":" + cfg.sshPort);
            session = connect();
            cb.step(0, total, UNINSTALL_STEPS[0], "ok", "connected");

            cb.step(1, total, UNINSTALL_STEPS[1], "run", null);
            exec(session, "systemctl disable --now " + SERVICE + " 2>/dev/null; true", null);
            cb.step(1, total, UNINSTALL_STEPS[1], "ok", "stopped & disabled");

            cb.step(2, total, UNINSTALL_STEPS[2], "run", null);
            exec(session, "rm -f " + UNIT_PATH + " " + REMOTE_BIN + "; systemctl daemon-reload; true", null);
            String active = exec(session, "systemctl is-active " + SERVICE + " 2>/dev/null", null).out.trim();
            cb.step(2, total, UNINSTALL_STEPS[2], "ok", "removed");
            cb.done(true, "Removed from server (is-active=" + (active.isEmpty() ? "inactive" : active) + ").");
        } catch (Throwable t) {
            cb.done(false, "Error: " + t.getMessage());
        } finally {
            if (session != null) session.disconnect();
        }
    }

    private Session connect() throws Exception {
        JSch jsch = new JSch();
        Session s = jsch.getSession(cfg.sshUser, cfg.sshHost, cfg.sshPort);
        s.setPassword(cfg.sshPass);
        Properties p = new Properties();
        p.put("StrictHostKeyChecking", "no");
        s.setConfig(p);
        s.setConfig("PreferredAuthentications", "password,keyboard-interactive");
        s.connect(15000);
        return s;
    }

    private void run() {
        int total = STEP_NAMES.length;
        Session session = null;
        try {
            // 0 — connect
            cb.step(0, total, STEP_NAMES[0], "run", cfg.sshUser + "@" + cfg.sshHost + ":" + cfg.sshPort);
            JSch jsch = new JSch();
            session = jsch.getSession(cfg.sshUser, cfg.sshHost, cfg.sshPort);
            session.setPassword(cfg.sshPass);
            Properties p = new Properties();
            p.put("StrictHostKeyChecking", "no");
            session.setConfig(p);
            session.setConfig("PreferredAuthentications", "password,keyboard-interactive");
            session.connect(15000);
            cb.step(0, total, STEP_NAMES[0], "ok", "connected");

            // 1 — arch
            cb.step(1, total, STEP_NAMES[1], "run", null);
            Exec u = exec(session, "uname -m", null);
            String uname = u.out.trim();
            String arch;
            if (uname.contains("x86_64") || uname.contains("amd64")) arch = "amd64";
            else if (uname.contains("aarch64") || uname.contains("arm64")) arch = "arm64";
            else { fail(1, total, STEP_NAMES[1], "unsupported arch: " + uname); return; }
            cb.step(1, total, STEP_NAMES[1], "ok", uname + " → " + arch);

            // 2 — upload
            cb.step(2, total, STEP_NAMES[2], "run", "streaming binary");
            byte[] bin = readAsset("server/notes-mail-tunnel-" + arch);
            // stop a running instance first so the binary isn't "text file busy"
            exec(session, "systemctl stop " + SERVICE + " 2>/dev/null; true", null);
            Exec up = exec(session, "cat > " + REMOTE_BIN + " && chmod 755 " + REMOTE_BIN + " && echo OK", bin);
            if (up.code != 0 || !up.out.contains("OK")) { fail(2, total, STEP_NAMES[2], trim(up)); return; }
            cb.step(2, total, STEP_NAMES[2], "ok", bin.length / 1024 + " KiB → " + REMOTE_BIN);

            // 3 — unit
            cb.step(3, total, STEP_NAMES[3], "run", null);
            String unit = buildUnit();
            Exec un = exec(session, "cat > " + UNIT_PATH + " && echo OK", unit.getBytes("UTF-8"));
            if (un.code != 0 || !un.out.contains("OK")) { fail(3, total, STEP_NAMES[3], trim(un)); return; }
            cb.step(3, total, STEP_NAMES[3], "ok", UNIT_PATH);

            // 4 — enable & start
            cb.step(4, total, STEP_NAMES[4], "run", "daemon-reload + enable --now");
            Exec en = exec(session, "systemctl daemon-reload && systemctl enable --now " + SERVICE
                    + " && systemctl restart " + SERVICE + " && echo OK", null);
            if (en.code != 0) { fail(4, total, STEP_NAMES[4], trim(en)); return; }
            cb.step(4, total, STEP_NAMES[4], "ok", "started");

            // 5 — verify
            cb.step(5, total, STEP_NAMES[5], "run", "checking status + logs");
            sleep(3500);
            String active = exec(session, "systemctl is-active " + SERVICE, null).out.trim();
            String logs = exec(session, "journalctl -u " + SERVICE + " --no-pager -n 12 2>&1", null).out;
            boolean authed = logs.contains("auth OK") || logs.contains("server ready");
            if ("active".equals(active) && authed) {
                cb.step(5, total, STEP_NAMES[5], "ok", "active · authenticated");
                cb.done(true, "Server is running: active, auth OK.");
            } else if ("active".equals(active)) {
                cb.step(5, total, STEP_NAMES[5], "ok", "active (auth not confirmed yet)");
                cb.done(true, "Service is active. Recent log:\n" + tail(logs));
            } else {
                cb.step(5, total, STEP_NAMES[5], "fail", "is-active=" + active);
                cb.done(false, "Service not active. Recent log:\n" + tail(logs));
            }
        } catch (Throwable t) {
            // mark whichever step is running as failed via a generic message
            cb.done(false, "Error: " + t.getMessage());
        } finally {
            if (session != null) session.disconnect();
        }
    }

    private String buildUnit() {
        StringBuilder e = new StringBuilder();
        e.append(REMOTE_BIN).append(" -server");
        e.append(" -url '").append(cfg.url).append("'");
        e.append(" -pipe '").append(esc(cfg.pipe)).append("'");
        e.append(" -token '").append(esc(cfg.token)).append("'");
        e.append(" -transport ").append(cfg.transport);
        e.append(" -lanes ").append(cfg.lanes);
        e.append(" -sockets ").append(cfg.sockets);
        e.append(" -window ").append(cfg.window);
        e.append(" -chunk ").append(cfg.chunk);
        e.append(" -writebuf ").append(cfg.writebuf);
        e.append(" -state-cap ").append(cfg.statecap);
        e.append(" -republish ").append(cfg.republish);
        e.append(" -cwnd ").append(cfg.cwnd);
        if (cfg.base64) e.append(" -base64");
        if (cfg.debug) e.append(" -debug");

        return "[Unit]\n"
                + "Description=Notes Mail Tunnel (server)\n"
                + "After=network-online.target\n"
                + "Wants=network-online.target\n\n"
                + "[Service]\n"
                + "ExecStart=" + e + "\n"
                + "Restart=always\n"
                + "RestartSec=3\n"
                + "User=root\n"
                + "LimitNOFILE=1048576\n\n"
                + "[Install]\n"
                + "WantedBy=multi-user.target\n";
    }

    /** systemd treats '$' as an env expansion; '$$' is the literal '$'. */
    private static String esc(String s) {
        return s == null ? "" : s.replace("$", "$$");
    }

    // ---- ssh exec helper ----
    private static class Exec { String out; int code; }

    private Exec exec(Session session, String cmd, byte[] stdin) throws Exception {
        ChannelExec ch = (ChannelExec) session.openChannel("exec");
        ch.setCommand(cmd);
        ch.setPty(false);
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        ch.setOutputStream(out);
        ch.setErrStream(out);
        OutputStream in = stdin != null ? ch.getOutputStream() : null;
        ch.connect(15000);
        if (stdin != null) {
            in.write(stdin);
            in.flush();
            in.close();
        }
        // wait for completion
        long deadline = System.currentTimeMillis() + 120000;
        while (!ch.isClosed() && System.currentTimeMillis() < deadline) sleep(80);
        Exec e = new Exec();
        e.out = out.toString("UTF-8");
        e.code = ch.getExitStatus();
        ch.disconnect();
        return e;
    }

    private byte[] readAsset(String path) throws Exception {
        AssetManager am = ctx.getAssets();
        try (InputStream is = am.open(path)) {
            ByteArrayOutputStream bos = new ByteArrayOutputStream(8 << 20);
            byte[] buf = new byte[65536];
            int n;
            while ((n = is.read(buf)) != -1) bos.write(buf, 0, n);
            return bos.toByteArray();
        }
    }

    private void fail(int idx, int total, String name, String detail) {
        cb.step(idx, total, name, "fail", detail);
        cb.done(false, name + " failed: " + detail);
    }

    private static String trim(Exec e) {
        String s = (e.out == null ? "" : e.out.trim());
        return s.length() > 300 ? s.substring(0, 300) : s;
    }

    private static String tail(String s) {
        if (s == null) return "";
        String[] lines = s.trim().split("\n");
        int from = Math.max(0, lines.length - 6);
        StringBuilder b = new StringBuilder();
        for (int i = from; i < lines.length; i++) b.append(lines[i]).append('\n');
        return b.toString();
    }

    private static void sleep(long ms) {
        try { Thread.sleep(ms); } catch (InterruptedException ignored) {}
    }
}
