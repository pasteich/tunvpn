package com.tun.vpn;

import android.annotation.SuppressLint;
import android.graphics.Bitmap;
import android.os.Bundle;
import android.webkit.CookieManager;
import android.webkit.JavascriptInterface;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Toast;

import androidx.annotation.Nullable;
import androidx.appcompat.app.AppCompatActivity;

import com.google.android.material.appbar.MaterialToolbar;

/**
 * Auto-grab the tunnel's -pipe / -token by letting the user log in to
 * notes.mail.ru in a real WebView (real HTTPS — 2FA/captcha/passkey all work),
 * then injecting a WebSocket sniffer (same idea as tools/tampermonkey.txt) that
 * captures the pipe UUID and token and hands them back to the app.
 *
 * This is the robust equivalent of the "login relay / TLS-terminating proxy"
 * idea, without the content-rewriting / CSP / mixed-content / WebAuthn breakage.
 */
public class LoginActivity extends AppCompatActivity {

    private static final String URL = "https://notes.mail.ru/";

    // WebSocket hook: extract pipe (UUID) + token (o:<hex>.c:<digits>) and call back.
    private static final String HOOK =
            "(function(){try{"
          + "if(window.__grab)return;window.__grab=1;"
          + "var N=window.WebSocket;if(!N)return;"
          + "var C={p:null,t:null,done:0};"
          + "var RT=/o:[0-9a-f]+\\.c:\\d+/i,RU=/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;"
          + "function scan(s,url){if(!s)s='';"
          + "if(!C.t){var m=s.match(RT);if(m)C.t=m[0];}"
          + "if(!C.p){var m2=(url||'').match(RU)||s.match(RU);if(m2)C.p=m2[0];}"
          + "if(C.p&&C.t&&!C.done){C.done=1;try{Android.onCreds('$'+C.p,C.t);}catch(e){}}}"
          + "function dec(d,url){try{"
          + "if(typeof d==='string')return scan(d,url);"
          + "if(d instanceof ArrayBuffer)return scan(new TextDecoder().decode(new Uint8Array(d)),url);"
          + "if(d&&d.buffer)return scan(new TextDecoder().decode(new Uint8Array(d.buffer)),url);"
          + "if(d&&d.arrayBuffer)return void d.arrayBuffer().then(function(b){scan(new TextDecoder().decode(new Uint8Array(b)),url);});"
          + "}catch(e){}}"
          + "function W(u,p){var ws=p!==undefined?new N(u,p):new N(u);"
          + "try{var mu=String(u).match(RU);if(mu&&!C.p)C.p=mu[0];}catch(e){}"
          + "ws.addEventListener('message',function(ev){dec(ev.data,String(u));});"
          + "var os=ws.send;ws.send=function(d){try{dec(d,String(u));}catch(e){}return os.apply(ws,arguments);};"
          + "return ws;}"
          + "W.prototype=N.prototype;['CONNECTING','OPEN','CLOSING','CLOSED'].forEach(function(k,i){W[k]=i;});"
          + "try{Object.defineProperty(W,'name',{value:'WebSocket'});}catch(e){}"
          + "window.WebSocket=W;"
          + "}catch(e){}})();";

    private WebView web;
    private volatile boolean captured = false;

    @SuppressLint("SetJavaScriptEnabled")
    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_login);

        MaterialToolbar tb = findViewById(R.id.login_toolbar);
        tb.setNavigationOnClickListener(v -> finish());
        tb.setOnMenuItemClickListener(item -> {
            if (item.getItemId() == R.id.action_reset) {
                CookieManager.getInstance().removeAllCookies(null);
                web.clearCache(true);
                web.loadUrl(URL);
                Toast.makeText(this, "Logged out — log in again", Toast.LENGTH_SHORT).show();
                return true;
            }
            return false;
        });

        web = findViewById(R.id.login_web);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setDatabaseEnabled(true);
        CookieManager.getInstance().setAcceptCookie(true);
        CookieManager.getInstance().setAcceptThirdPartyCookies(web, true);

        web.addJavascriptInterface(new Bridge(), "Android");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageStarted(WebView v, String url, Bitmap fav) {
                v.evaluateJavascript(HOOK, null);
            }
            @Override
            public void onPageFinished(WebView v, String url) {
                v.evaluateJavascript(HOOK, null);
            }
            @Override
            public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest r) {
                return false; // keep navigation inside the WebView
            }
        });

        web.loadUrl(URL);
        Toast.makeText(this, "Log in and open a note — creds are grabbed automatically", Toast.LENGTH_LONG).show();
    }

    private class Bridge {
        @JavascriptInterface
        public void onCreds(String pipe, String token) {
            if (captured) return;
            captured = true;
            runOnUiThread(() -> {
                Config c = Config.load(LoginActivity.this);
                c.pipe = pipe;
                c.token = token;
                c.save(LoginActivity.this);
                Toast.makeText(LoginActivity.this, "Creds captured ✓", Toast.LENGTH_SHORT).show();
                setResult(RESULT_OK);
                finish();
            });
        }
    }

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }
}
