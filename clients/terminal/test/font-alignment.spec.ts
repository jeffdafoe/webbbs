import { test, expect } from '@playwright/test';

test('characters align correctly in xterm with letter-spacing override', async ({ page }) => {
    await page.goto('http://localhost:4202');
    await page.waitForSelector('.xterm-rows');
    await page.waitForTimeout(3000);

    // Check the xterm DOM spans for letter-spacing values
    const spanInfo = await page.evaluate(() => {
        const allSpans = document.querySelectorAll('.xterm-rows span');
        const results: { text: string; letterSpacing: string; computedLS: string; width: number }[] = [];
        allSpans.forEach(span => {
            const el = span as HTMLElement;
            const text = el.textContent || '';
            if (text.trim().length > 0) {
                results.push({
                    text: text.substring(0, 30),
                    letterSpacing: el.style.letterSpacing,
                    computedLS: getComputedStyle(el).letterSpacing,
                    width: el.offsetWidth
                });
            }
        });
        return results;
    });

    console.log('Xterm spans with content:');
    for (const s of spanInfo) {
        console.log(`  "${s.text}" inline-ls="${s.letterSpacing}" computed-ls="${s.computedLS}" width=${s.width}`);
    }

    // Check that all computed letter-spacing is 0px (our CSS override working)
    for (const s of spanInfo) {
        expect(s.computedLS).toBe('0px');
    }

    await page.screenshot({ path: 'test/screenshots/xterm-content.png' });
});

test('standalone alignment grid renders correctly', async ({ page }) => {
    // Create a standalone page that loads the font from vite and renders a test grid
    await page.goto('http://localhost:4202');
    await page.waitForSelector('.xterm-rows');
    await page.waitForTimeout(1000);

    // Now navigate to a data URL but first load the font
    await page.evaluate(() => {
        document.body.innerHTML = `
            <div id="grid" style="
                background: #0a0a1a;
                color: #c0c0c0;
                font-family: 'ZBBS VGA', monospace;
                font-size: 20px;
                white-space: pre;
                font-kerning: none;
                letter-spacing: 0px;
                margin: 20px;
            "></div>
        `;

        const grid = document.getElementById('grid')!;
        const lines = [];

        lines.push('A'.repeat(80));
        lines.push('='.repeat(80));
        lines.push('-'.repeat(80));
        lines.push('_'.repeat(80));
        lines.push('#'.repeat(80));
        // Ruler with markers
        const ruler = '|' + ' '.repeat(18) + '|' + ' '.repeat(18) + '|' + ' '.repeat(18) + '|' + ' '.repeat(18) + '|' + ' '.repeat(3) + '|';
        lines.push(ruler);
        // Mixed row
        lines.push('ABCDEFGHIJ' + '==========' + '----------' + '__________' + '##########' + 'KLMNOPQRST' + '0123456789' + '++++++++++');
        // Numbers
        lines.push('0123456789'.repeat(8));

        grid.textContent = lines.join('\n');
    });

    await page.waitForTimeout(500);
    await page.screenshot({ path: 'test/screenshots/alignment-grid.png' });

    // Measure the width of each line using getBoundingClientRect for subpixel precision
    const rowWidths = await page.evaluate(() => {
        const grid = document.getElementById('grid')!;
        const text = grid.textContent || '';
        const lines = text.split('\n');

        const results: { line: number; firstChar: string; totalWidth: number; perChar: number }[] = [];
        for (let i = 0; i < lines.length; i++) {
            const span = document.createElement('span');
            span.style.fontFamily = "'ZBBS VGA', monospace";
            span.style.fontSize = '20px';
            span.style.whiteSpace = 'pre';
            span.style.fontKerning = 'none';
            span.style.letterSpacing = '0px';
            span.style.position = 'absolute';
            span.textContent = lines[i];
            document.body.appendChild(span);
            const rect = span.getBoundingClientRect();
            results.push({
                line: i,
                firstChar: lines[i].charAt(0),
                totalWidth: rect.width,
                perChar: rect.width / lines[i].length
            });
            document.body.removeChild(span);
        }
        return results;
    });

    console.log('Row width measurements (getBoundingClientRect):');
    const expectedWidth = rowWidths[0]?.totalWidth;
    let failures: string[] = [];
    for (const row of rowWidths) {
        const diff = Math.abs(row.totalWidth - expectedWidth);
        const match = diff < 1;
        const status = match ? 'OK' : 'MISMATCH';
        console.log(`  Line ${row.line} ('${row.firstChar}'): total=${row.totalWidth.toFixed(2)}px per-char=${row.perChar.toFixed(4)}px diff=${diff.toFixed(2)} ${status}`);
        if (!match) {
            failures.push(`Line ${row.line} ('${row.firstChar}'): ${row.totalWidth.toFixed(2)}px vs expected ${expectedWidth.toFixed(2)}px`);
        }
    }

    if (failures.length > 0) {
        console.log(`\nFAILED - ${failures.length} rows have different widths:`);
        failures.forEach(f => console.log(`  ${f}`));
    }

    expect(failures).toEqual([]);
});
