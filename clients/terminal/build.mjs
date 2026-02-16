import * as esbuild from 'esbuild';
import { cpSync, mkdirSync, readFileSync, writeFileSync } from 'fs';
import { createHash } from 'crypto';

await esbuild.build({
    entryPoints: ['src/main.ts'],
    bundle: true,
    outdir: 'dist',
    format: 'esm',
    loader: {
        '.ans': 'text',
    },
    minify: true,
    sourcemap: true,
});

// Copy static assets
mkdirSync('dist/fonts', { recursive: true });
cpSync('public/fonts', 'dist/fonts', { recursive: true });
cpSync('index.html', 'dist/index.html');

// Cache-bust: append content hash to main.js script tag
const jsContent = readFileSync('dist/main.js');
const hash = createHash('md5').update(jsContent).digest('hex').substring(0, 8);
let html = readFileSync('dist/index.html', 'utf-8');
html = html.replace('src="main.js"', `src="main.js?v=${hash}"`);
writeFileSync('dist/index.html', html);

console.log('Build complete.');
