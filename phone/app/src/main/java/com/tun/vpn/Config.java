package com.tun.vpn;

import android.content.Context;
import android.content.SharedPreferences;

/** Holds all tunnel + VPN settings and persists them in SharedPreferences. */
public class Config {
    public String url = "wss://notes.mail.ru/ws/pipe?platform=web";
    public String pipe = "";
    public String token = "";
    public String cookie = "";
    public String transport = "sync"; // awareness | sync

    public int lanes = 1;
    public int sockets = 1;
    public int window = 4;
    public int chunk = 16000;
    public int writebuf = 512;
    public int statecap = 4000;
    public String republish = "500ms";
    public int cwnd = 5500000;
    public boolean base64 = false;
    public boolean debug = false;

    public String dns = "1.1.1.1";
    public int mtu = 1500;
    public int port = 8888;
    public boolean blockAAAA = true;

    // SSH deploy (server tab)
    public String sshHost = "";
    public int sshPort = 22;
    public String sshUser = "root";
    public String sshPass = "";

    private static final String PREFS = "tunvpn";

    public static Config load(Context ctx) {
        SharedPreferences p = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        Config c = new Config();
        c.url = p.getString("url", c.url);
        c.pipe = p.getString("pipe", c.pipe);
        c.token = p.getString("token", c.token);
        c.cookie = p.getString("cookie", c.cookie);
        c.transport = p.getString("transport", c.transport);
        c.lanes = p.getInt("lanes", c.lanes);
        c.sockets = p.getInt("sockets", c.sockets);
        c.window = p.getInt("window", c.window);
        c.chunk = p.getInt("chunk", c.chunk);
        c.writebuf = p.getInt("writebuf", c.writebuf);
        c.statecap = p.getInt("statecap", c.statecap);
        c.republish = p.getString("republish", c.republish);
        c.cwnd = p.getInt("cwnd", c.cwnd);
        c.base64 = p.getBoolean("base64", c.base64);
        c.debug = p.getBoolean("debug", c.debug);
        c.dns = p.getString("dns", c.dns);
        c.mtu = p.getInt("mtu", c.mtu);
        c.port = p.getInt("port", c.port);
        c.blockAAAA = p.getBoolean("blockAAAA", c.blockAAAA);
        c.sshHost = p.getString("sshHost", c.sshHost);
        c.sshPort = p.getInt("sshPort", c.sshPort);
        c.sshUser = p.getString("sshUser", c.sshUser);
        c.sshPass = p.getString("sshPass", c.sshPass);
        return c;
    }

    public void save(Context ctx) {
        SharedPreferences.Editor e = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit();
        e.putString("url", url);
        e.putString("pipe", pipe);
        e.putString("token", token);
        e.putString("cookie", cookie);
        e.putString("transport", transport);
        e.putInt("lanes", lanes);
        e.putInt("sockets", sockets);
        e.putInt("window", window);
        e.putInt("chunk", chunk);
        e.putInt("writebuf", writebuf);
        e.putInt("statecap", statecap);
        e.putString("republish", republish);
        e.putInt("cwnd", cwnd);
        e.putBoolean("base64", base64);
        e.putBoolean("debug", debug);
        e.putString("dns", dns);
        e.putInt("mtu", mtu);
        e.putInt("port", port);
        e.putBoolean("blockAAAA", blockAAAA);
        e.putString("sshHost", sshHost);
        e.putInt("sshPort", sshPort);
        e.putString("sshUser", sshUser);
        e.putString("sshPass", sshPass);
        e.apply();
    }

    /** Serialize into an Intent extra bundle keys. */
    public void toIntent(android.content.Intent i) {
        i.putExtra("url", url);
        i.putExtra("pipe", pipe);
        i.putExtra("token", token);
        i.putExtra("cookie", cookie);
        i.putExtra("transport", transport);
        i.putExtra("lanes", lanes);
        i.putExtra("sockets", sockets);
        i.putExtra("window", window);
        i.putExtra("chunk", chunk);
        i.putExtra("writebuf", writebuf);
        i.putExtra("statecap", statecap);
        i.putExtra("republish", republish);
        i.putExtra("cwnd", cwnd);
        i.putExtra("base64", base64);
        i.putExtra("debug", debug);
        i.putExtra("dns", dns);
        i.putExtra("mtu", mtu);
        i.putExtra("port", port);
        i.putExtra("blockAAAA", blockAAAA);
    }

    public static Config fromIntent(android.content.Intent i) {
        Config c = new Config();
        c.url = i.getStringExtra("url");
        c.pipe = i.getStringExtra("pipe");
        c.token = i.getStringExtra("token");
        c.cookie = i.getStringExtra("cookie");
        c.transport = i.getStringExtra("transport");
        c.lanes = i.getIntExtra("lanes", c.lanes);
        c.sockets = i.getIntExtra("sockets", c.sockets);
        c.window = i.getIntExtra("window", c.window);
        c.chunk = i.getIntExtra("chunk", c.chunk);
        c.writebuf = i.getIntExtra("writebuf", c.writebuf);
        c.statecap = i.getIntExtra("statecap", c.statecap);
        c.republish = i.getStringExtra("republish");
        c.cwnd = i.getIntExtra("cwnd", c.cwnd);
        c.base64 = i.getBooleanExtra("base64", c.base64);
        c.debug = i.getBooleanExtra("debug", c.debug);
        c.dns = i.getStringExtra("dns");
        c.mtu = i.getIntExtra("mtu", c.mtu);
        c.port = i.getIntExtra("port", c.port);
        c.blockAAAA = i.getBooleanExtra("blockAAAA", c.blockAAAA);
        return c;
    }
}
