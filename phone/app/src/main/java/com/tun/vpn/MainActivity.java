package com.tun.vpn;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.view.View;
import android.widget.ScrollView;
import android.widget.TextView;

import androidx.annotation.Nullable;
import androidx.appcompat.app.AppCompatActivity;

import com.google.android.material.button.MaterialButton;
import com.google.android.material.materialswitch.MaterialSwitch;
import com.google.android.material.progressindicator.LinearProgressIndicator;
import com.google.android.material.textfield.MaterialAutoCompleteTextView;
import com.google.android.material.textfield.TextInputEditText;

import java.util.ArrayList;
import java.util.List;

public class MainActivity extends AppCompatActivity implements TunState.Listener {

    private static final int REQ_VPN = 1001;
    private static final int REQ_NOTIF = 1002;

    // tunnel
    private TextInputEditText etUrl, etPipe, etToken, etCookie, etLanes, etSockets, etWindow,
            etChunk, etWritebuf, etStatecap, etRepublish, etCwnd, etDns, etMtu, etPort;
    private MaterialAutoCompleteTextView ddTransport;
    private MaterialSwitch swBase64, swDebug, swBlockAaaa;
    private MaterialButton btnToggle, btnClear;
    private TextView tvStatus, tvLog, tvConn, chevAdvanced;
    private ScrollView logScroll;
    private View boxAdvanced, hdrAdvanced;
    private MatrixRainView matrix;
    // server
    private TextInputEditText etSshHost, etSshUser, etSshPort, etSshPass;
    private MaterialButton btnDeploy, btnUpdateCreds, btnUninstall, btnCheck;
    private LinearProgressIndicator deployProgress;
    private View deploySteps, hdrParams, boxParams;
    private TextView deployStatus, deployHint, chevParams;
    // tabs
    private View tabTunnel, tabServer, tabLog;

    private final List<TextView> stepRows = new ArrayList<>();
    private volatile boolean deploying = false;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        etUrl = f(R.id.et_url); etPipe = f(R.id.et_pipe); etToken = f(R.id.et_token);
        etCookie = f(R.id.et_cookie); etLanes = f(R.id.et_lanes); etSockets = f(R.id.et_sockets);
        etWindow = f(R.id.et_window); etChunk = f(R.id.et_chunk); etWritebuf = f(R.id.et_writebuf);
        etStatecap = f(R.id.et_statecap); etRepublish = f(R.id.et_republish); etCwnd = f(R.id.et_cwnd);
        etDns = f(R.id.et_dns); etMtu = f(R.id.et_mtu); etPort = f(R.id.et_port);
        ddTransport = findViewById(R.id.dd_transport);
        swBase64 = findViewById(R.id.sw_base64); swDebug = findViewById(R.id.sw_debug);
        swBlockAaaa = findViewById(R.id.sw_block_aaaa);
        btnToggle = findViewById(R.id.btn_toggle); btnClear = findViewById(R.id.btn_clear);
        tvStatus = findViewById(R.id.tv_status); tvLog = findViewById(R.id.tv_log);
        logScroll = findViewById(R.id.log_scroll);
        tvConn = findViewById(R.id.tv_conn);
        chevAdvanced = findViewById(R.id.chev_advanced);
        boxAdvanced = findViewById(R.id.box_advanced); hdrAdvanced = findViewById(R.id.hdr_advanced);
        matrix = findViewById(R.id.matrix);

        etSshHost = f(R.id.et_ssh_host); etSshUser = f(R.id.et_ssh_user);
        etSshPort = f(R.id.et_ssh_port); etSshPass = f(R.id.et_ssh_pass);
        btnDeploy = findViewById(R.id.btn_deploy); btnUpdateCreds = findViewById(R.id.btn_update_creds);
        btnUninstall = findViewById(R.id.btn_uninstall); btnCheck = findViewById(R.id.btn_check);
        deployProgress = findViewById(R.id.deploy_progress); deploySteps = findViewById(R.id.deploy_steps);
        deployStatus = findViewById(R.id.deploy_status); deployHint = findViewById(R.id.deploy_hint);
        hdrParams = findViewById(R.id.hdr_params); boxParams = findViewById(R.id.box_params);
        chevParams = findViewById(R.id.chev_params);

        tabTunnel = findViewById(R.id.tab_tunnel); tabServer = findViewById(R.id.tab_server);
        tabLog = findViewById(R.id.tab_log);

        ddTransport.setSimpleItems(new String[]{"awareness", "sync"});
        populate(Config.load(this));
        renderLogs();

        com.google.android.material.bottomnavigation.BottomNavigationView bn = findViewById(R.id.bottom_nav);
        bn.setOnItemSelectedListener(item -> {
            int id = item.getItemId();
            tabTunnel.setVisibility(id == R.id.nav_tunnel ? View.VISIBLE : View.GONE);
            tabServer.setVisibility(id == R.id.nav_server ? View.VISIBLE : View.GONE);
            tabLog.setVisibility(id == R.id.nav_log ? View.VISIBLE : View.GONE);
            if (id == R.id.nav_log) renderLogs();
            return true;
        });

        btnToggle.setOnClickListener(v -> {
            boolean active = TunState.isRunning() || !TunState.DISCONNECTED.equals(TunState.phase());
            if (active) stopVpn(); else startVpn();
        });
        btnClear.setOnClickListener(v -> { TunState.clear(); tvLog.setText(""); });
        hdrAdvanced.setOnClickListener(v -> {
            boolean show = boxAdvanced.getVisibility() != View.VISIBLE;
            boxAdvanced.setVisibility(show ? View.VISIBLE : View.GONE);
            chevAdvanced.setText(show ? "▾" : "▸");
        });
        hdrParams.setOnClickListener(v -> {
            boolean show = boxParams.getVisibility() != View.VISIBLE;
            boxParams.setVisibility(show ? View.VISIBLE : View.GONE);
            chevParams.setText(show ? "▾" : "▸");
        });

        btnDeploy.setOnClickListener(v -> runDeploy(0));
        btnUpdateCreds.setOnClickListener(v -> runDeploy(1));
        btnUninstall.setOnClickListener(v -> confirmUninstall());
        btnCheck.setOnClickListener(v -> checkServer());

        requestNotifPermission();
    }

    private <T extends View> T f(int id) { return findViewById(id); }

    private void populate(Config c) {
        etUrl.setText(c.url); etPipe.setText(c.pipe); etToken.setText(c.token); etCookie.setText(c.cookie);
        ddTransport.setText(c.transport, false);
        etLanes.setText(s(c.lanes)); etSockets.setText(s(c.sockets)); etWindow.setText(s(c.window));
        etChunk.setText(s(c.chunk)); etWritebuf.setText(s(c.writebuf)); etStatecap.setText(s(c.statecap));
        etRepublish.setText(c.republish); etCwnd.setText(s(c.cwnd));
        swBase64.setChecked(c.base64); swDebug.setChecked(c.debug); swBlockAaaa.setChecked(c.blockAAAA);
        etDns.setText(c.dns); etMtu.setText(s(c.mtu)); etPort.setText(s(c.port));
        etSshHost.setText(c.sshHost); etSshUser.setText(c.sshUser);
        etSshPort.setText(s(c.sshPort)); etSshPass.setText(c.sshPass);
    }

    private Config gather() {
        Config c = new Config();
        c.url = str(etUrl, c.url); c.pipe = str(etPipe, ""); c.token = str(etToken, "");
        c.cookie = str(etCookie, "");
        c.transport = ddTransport.getText().toString().trim();
        if (c.transport.isEmpty()) c.transport = "sync";
        c.lanes = num(etLanes, c.lanes); c.sockets = num(etSockets, c.sockets);
        c.window = num(etWindow, c.window); c.chunk = num(etChunk, c.chunk);
        c.writebuf = num(etWritebuf, c.writebuf); c.statecap = num(etStatecap, c.statecap);
        c.republish = str(etRepublish, c.republish); c.cwnd = num(etCwnd, c.cwnd);
        c.base64 = swBase64.isChecked(); c.debug = swDebug.isChecked(); c.blockAAAA = swBlockAaaa.isChecked();
        c.dns = str(etDns, c.dns); c.mtu = num(etMtu, c.mtu); c.port = num(etPort, c.port);
        c.sshHost = str(etSshHost, ""); c.sshUser = str(etSshUser, "root");
        c.sshPort = num(etSshPort, 22); c.sshPass = etSshPass.getText() == null ? "" : etSshPass.getText().toString();
        return c;
    }

    private String s(int v) { return String.valueOf(v); }
    private String str(TextInputEditText e, String def) {
        String v = e.getText() == null ? "" : e.getText().toString().trim();
        return v.isEmpty() ? def : v;
    }
    private int num(TextInputEditText e, int def) {
        try { return Integer.parseInt(e.getText().toString().trim()); } catch (Exception x) { return def; }
    }

    // ---------------- VPN ----------------
    private void startVpn() {
        Config c = gather();
        if (c.pipe.isEmpty() || c.token.isEmpty()) { tvConn.setText("● need pipe/token"); return; }
        c.save(this);
        Intent prep = VpnService.prepare(this);
        if (prep != null) startActivityForResult(prep, REQ_VPN);
        else onActivityResult(REQ_VPN, RESULT_OK, null);
    }

    private void stopVpn() {
        Intent i = new Intent(this, TunVpnService.class).setAction(TunVpnService.ACTION_STOP);
        startService(i);
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, @Nullable Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == REQ_VPN && resultCode == Activity.RESULT_OK) {
            Config c = gather(); c.save(this);
            TunState.clear(); tvLog.setText("");
            Intent i = new Intent(this, TunVpnService.class).setAction(TunVpnService.ACTION_START);
            c.toIntent(i);
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) startForegroundService(i); else startService(i);
        } else if (requestCode == REQ_VPN) {
            tvConn.setText("● vpn denied");
        }
    }

    // ---------------- Check server ----------------
    private void checkServer() {
        Config c = gather();
        if (c.sshHost.isEmpty() || c.sshPass.isEmpty()) { deployHint.setText("Fill server host and SSH password."); return; }
        c.save(this);
        deployHint.setText("Checking " + c.sshHost + "…");
        new DeployManager(this, c, null).checkServer((active, text) -> runOnUiThread(() -> {
            TextView tv = new TextView(this);
            int pad = (int) (16 * getResources().getDisplayMetrics().density);
            tv.setPadding(pad, pad, pad, pad);
            tv.setText(text);
            tv.setTextSize(11);
            tv.setTypeface(android.graphics.Typeface.MONOSPACE);
            tv.setTextIsSelectable(true);
            ScrollView sv = new ScrollView(this);
            sv.addView(tv);
            deployHint.setText(active ? "Server: active ✓" : "Server: not active ✗");
            new AlertDialog.Builder(this)
                    .setTitle(active ? "Server active ✓" : "Server ✗")
                    .setView(sv)
                    .setPositiveButton("Close", null)
                    .show();
        }));
    }

    // ---------------- Deploy ----------------
    // mode: 0 deploy, 1 update creds
    private void runDeploy(int mode) {
        if (deploying) return;
        Config c = gather();
        if (c.sshHost.isEmpty() || c.sshPass.isEmpty()) { deployHint.setText("Fill server host and SSH password."); return; }
        if (c.pipe.isEmpty() || c.token.isEmpty()) { deployHint.setText("Set Pipe key / Token on the Tunnel tab first."); return; }
        c.save(this);
        beginDeployUi(mode == 0 ? "Deploying…" : "Updating…");
        DeployManager dm = new DeployManager(this, c, deployCb());
        if (mode == 0) dm.start(); else dm.updateCreds();
    }

    private void confirmUninstall() {
        if (deploying) return;
        Config c = gather();
        if (c.sshHost.isEmpty() || c.sshPass.isEmpty()) { deployHint.setText("Fill server host and SSH password."); return; }
        new AlertDialog.Builder(this)
                .setTitle("Remove from server?")
                .setMessage("Stop and delete the systemd service and binary from " + c.sshHost + "?")
                .setPositiveButton("Remove", (d, w) -> {
                    c.save(this);
                    beginDeployUi("Removing…");
                    new DeployManager(this, c, deployCb()).uninstall();
                })
                .setNegativeButton("Cancel", null)
                .show();
    }

    private void beginDeployUi(String status) {
        deploying = true;
        btnDeploy.setEnabled(false); btnUpdateCreds.setEnabled(false); btnUninstall.setEnabled(false);
        ((android.widget.LinearLayout) deploySteps).removeAllViews();
        stepRows.clear();
        deployProgress.setProgress(0);
        deployStatus.setText(status);
        deployStatus.setTextColor(Color.parseColor("#888888"));
        deployHint.setText("");
    }

    private DeployManager.Cb deployCb() {
        return new DeployManager.Cb() {
            @Override public void step(int idx, int total, String name, String state, String detail) {
                runOnUiThread(() -> {
                    android.widget.LinearLayout box = (android.widget.LinearLayout) deploySteps;
                    while (stepRows.size() <= idx) {
                        TextView tv = new TextView(MainActivity.this);
                        tv.setTextSize(13);
                        tv.setPadding(0, 6, 0, 6);
                        box.addView(tv);
                        stepRows.add(tv);
                    }
                    String icon = "run".equals(state) ? "⏳" : "ok".equals(state) ? "✅" : "❌";
                    String line = icon + "  " + name + (detail != null && !detail.isEmpty() ? "  —  " + detail : "");
                    TextView row = stepRows.get(idx);
                    row.setText(line);
                    row.setTextColor("fail".equals(state) ? Color.parseColor("#E74C3C")
                            : "ok".equals(state) ? Color.parseColor("#2ECC71") : Color.parseColor("#B0B0B0"));
                    int pct = (int) (((idx + ("ok".equals(state) ? 1 : 0)) / (float) total) * 100);
                    deployProgress.setProgressCompat(pct, true);
                });
            }
            @Override public void done(boolean ok, String summary) {
                runOnUiThread(() -> {
                    deploying = false;
                    btnDeploy.setEnabled(true); btnUpdateCreds.setEnabled(true); btnUninstall.setEnabled(true);
                    if (ok) deployProgress.setProgressCompat(100, true);
                    deployStatus.setText(ok ? "Done ✓" : "Failed ✗");
                    deployStatus.setTextColor(ok ? Color.parseColor("#2ECC71") : Color.parseColor("#E74C3C"));
                    deployHint.setText(summary);
                    TunState.log("[deploy] " + (ok ? "OK: " : "FAIL: ") + summary);
                });
            }
        };
    }

    // ---------------- lifecycle / listener ----------------
    private void requestNotifPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIF);
        }
    }

    @Override protected void onResume() {
        super.onResume();
        TunState.setListener(this);
        onRunningChanged(TunState.isRunning());
        onPhase(TunState.phase(), TunState.phaseDetail());
        renderLogs();
        if (matrix != null) matrix.resume();
    }

    @Override protected void onPause() {
        super.onPause();
        TunState.setListener(null);
        if (matrix != null) matrix.pause();
    }

    private void renderLogs() {
        List<String> lines = TunState.snapshot();
        StringBuilder sb = new StringBuilder();
        for (String l : lines) sb.append(l).append('\n');
        tvLog.setText(sb.toString());
        scrollLogDown();
    }
    private void scrollLogDown() { logScroll.post(() -> logScroll.scrollTo(0, tvLog.getBottom())); }

    @Override public void onRunningChanged(boolean running) {
        btnToggle.setText(running ? "Stop VPN" : "Start VPN");
    }

    @Override public void onPhase(String phase, String detail) {
        String dot; int col;
        switch (phase) {
            case TunState.CONNECTED:
                dot = "● connected"; col = 0xFF7FB069; break;
            case TunState.CONNECTING:
            case TunState.STARTING:
                dot = "◐ connecting"; col = 0xFFB0824F; break;
            case TunState.AUTH_FAIL:
                dot = "✗ auth failed"; col = 0xFFD0674A; break;
            case TunState.NO_ROUTE:
                dot = "⚠ no route"; col = 0xFFD0674A; break;
            case TunState.ERROR:
                dot = "✗ error"; col = 0xFFD0674A; break;
            default:
                dot = "● offline"; col = 0xFFB9A588;
        }
        if (tvConn != null) { tvConn.setText(dot); tvConn.setTextColor(col); }
        if (tvStatus != null) { tvStatus.setText(dot); tvStatus.setTextColor(col); }
        btnToggle.setText(TunState.DISCONNECTED.equals(phase) ? "Start VPN" : "Stop VPN");
    }

    @Override public void onLog(String line) {
        tvLog.append(line + "\n");
        scrollLogDown();
    }
}
