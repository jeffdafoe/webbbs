import { test } from '@playwright/test';

test('canvas vs DOM font measurement', async ({ page }) => {
    await page.goto('http://localhost:4202');
    await page.waitForSelector('.xterm-rows');
    await page.waitForTimeout(5000);

    const info = await page.evaluate(() => {
        // Test 1: OffscreenCanvas (what xterm TextMetricsMeasureStrategy uses)
        const oc = new OffscreenCanvas(100, 100);
        const octx = oc.getContext('2d')!;
        octx.font = "20px 'ZBBS VGA', monospace";
        const om = octx.measureText('W');

        // Test 2: Regular canvas
        const c = document.createElement('canvas');
        const ctx = c.getContext('2d')!;
        ctx.font = "20px 'ZBBS VGA', monospace";
        const cm = ctx.measureText('W');

        // Test 3: OffscreenCanvas with fallback font only
        const oc2 = new OffscreenCanvas(100, 100);
        const octx2 = oc2.getContext('2d')!;
        octx2.font = "20px monospace";
        const om2 = octx2.measureText('W');

        return {
            offscreen: {
                width: om.width,
                ascent: om.fontBoundingBoxAscent,
                descent: om.fontBoundingBoxDescent,
                total: om.fontBoundingBoxAscent + om.fontBoundingBoxDescent,
            },
            regularCanvas: {
                width: cm.width,
                ascent: cm.fontBoundingBoxAscent,
                descent: cm.fontBoundingBoxDescent,
                total: cm.fontBoundingBoxAscent + cm.fontBoundingBoxDescent,
            },
            fallbackMonospace: {
                width: om2.width,
                ascent: om2.fontBoundingBoxAscent,
                descent: om2.fontBoundingBoxDescent,
                total: om2.fontBoundingBoxAscent + om2.fontBoundingBoxDescent,
            },
            firstRowHeight: (document.querySelector('.xterm-rows > div') as HTMLElement)?.style.height,
        };
    });

    console.log('OffscreenCanvas ZBBS VGA:', JSON.stringify(info.offscreen));
    console.log('Regular canvas ZBBS VGA:', JSON.stringify(info.regularCanvas));
    console.log('OffscreenCanvas monospace:', JSON.stringify(info.fallbackMonospace));
    console.log('Xterm row height:', info.firstRowHeight);
});
