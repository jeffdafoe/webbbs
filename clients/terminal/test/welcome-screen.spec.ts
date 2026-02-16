import { test } from '@playwright/test';

test('capture welcome screen', async ({ page }) => {
    page.on('console', msg => {
        if (msg.type() === 'error') {
            console.log('BROWSER ERROR:', msg.text());
        }
    });
    page.on('pageerror', err => {
        console.log('PAGE ERROR:', err.message);
    });

    await page.goto('http://localhost:4202');
    await page.waitForSelector('.xterm-rows');

    // Wait for actual content to appear in xterm rows
    // Poll until we see non-empty rows (up to 15 seconds)
    let attempts = 0;
    let contentFound = false;
    while (attempts < 30 && !contentFound) {
        await page.waitForTimeout(500);
        contentFound = await page.evaluate(() => {
            const rows = document.querySelectorAll('.xterm-rows > div');
            for (const row of rows) {
                if ((row.textContent || '').trim().length > 5) {
                    return true;
                }
            }
            return false;
        });
        attempts++;
    }

    console.log(`Content appeared after ${attempts * 500}ms`);

    // Extra wait for rendering to settle
    await page.waitForTimeout(1000);

    await page.screenshot({ path: 'test/screenshots/welcome.png' });

    // Also dump what we see
    const content = await page.evaluate(() => {
        const rows = document.querySelectorAll('.xterm-rows > div');
        const lines: string[] = [];
        rows.forEach((row, i) => {
            const text = row.textContent || '';
            if (text.trim()) {
                lines.push(`Row ${i}: [${text.trim().substring(0, 70)}]`);
            }
        });
        return lines;
    });
    content.forEach(l => console.log(l));
});
