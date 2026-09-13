package com.tun.vpn;

import android.content.Context;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.Typeface;
import android.util.AttributeSet;
import android.view.View;

import java.util.Random;

/** Matrix-style falling glyph rain, in warm (Claude clay/amber) tones. */
public class MatrixRainView extends View {

    private static final String GLYPHS =
            "0123456789ABCDEF＄＃＊＋=<>/\\ｱｲｳｴｵｶｷｸｹｺｻｼｽｾﾀﾁﾂﾃﾅﾆﾇﾈﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖ";
    private static final int TRAIL = 16;

    private final Paint head = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint tail = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Random rnd = new Random();

    private float cell = 34f;
    private int cols, rows;
    private float[] y;
    private float[] speed;
    private char[][] grid;
    private boolean running = true;

    public MatrixRainView(Context c, AttributeSet a) {
        super(c, a);
        float sp = getResources().getDisplayMetrics().scaledDensity;
        cell = 13f * sp;
        head.setColor(0xFFF3E7D6);   // warm near-white
        tail.setColor(0xFFC7764E);   // Claude clay
        head.setTextSize(cell * 0.92f);
        tail.setTextSize(cell * 0.92f);
        head.setTypeface(Typeface.MONOSPACE);
        tail.setTypeface(Typeface.MONOSPACE);
    }

    @Override
    protected void onSizeChanged(int w, int h, int ow, int oh) {
        super.onSizeChanged(w, h, ow, oh);
        cols = Math.max(1, (int) (w / cell));
        rows = Math.max(1, (int) (h / cell)) + 2;
        y = new float[cols];
        speed = new float[cols];
        grid = new char[cols][rows];
        for (int i = 0; i < cols; i++) {
            y[i] = rnd.nextInt(rows);
            speed[i] = 0.12f + rnd.nextFloat() * 0.45f;
            for (int j = 0; j < rows; j++) grid[i][j] = rndCh();
        }
    }

    private char rndCh() {
        return GLYPHS.charAt(rnd.nextInt(GLYPHS.length()));
    }

    @Override
    protected void onDraw(Canvas cv) {
        if (grid == null) return;
        for (int i = 0; i < cols; i++) {
            int headJ = (int) y[i];
            for (int t = 0; t < TRAIL; t++) {
                int j = headJ - t;
                if (j < 0 || j >= rows) continue;
                float x = i * cell;
                float yy = (j + 1) * cell;
                char ch = grid[i][j];
                if (t == 0) {
                    cv.drawText(String.valueOf(ch), x, yy, head);
                } else {
                    int alpha = (int) (210 * (1f - (t / (float) TRAIL)));
                    tail.setAlpha(Math.max(0, alpha));
                    cv.drawText(String.valueOf(ch), x, yy, tail);
                }
            }
            y[i] += speed[i];
            if (y[i] - TRAIL > rows) {
                y[i] = 0;
                speed[i] = 0.12f + rnd.nextFloat() * 0.45f;
            }
            if (rnd.nextInt(18) == 0) grid[i][rnd.nextInt(rows)] = rndCh();
        }
        if (running) postInvalidateOnAnimation();
    }

    public void pause() { running = false; }

    public void resume() {
        if (!running) { running = true; postInvalidateOnAnimation(); }
    }
}
