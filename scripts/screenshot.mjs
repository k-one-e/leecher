// screenshot.mjs — README screenshot of a local leecher with demo data.
//
// Run a throwaway instance on 127.0.0.1 with an account, then:
//   npm i playwright-core   (uses the installed Google Chrome)
//   LEECHER_URL=http://127.0.0.1:19090 LEECHER_USER=demo LEECHER_PASSWORD=... \
//     node scripts/screenshot.mjs assets/screenshot.jpeg
//
// The status pill, disk and format lookups are answered with demo data, so the
// image never shows the machine's real IP, location or disks.
import { chromium } from 'playwright-core';

const base = process.env.LEECHER_URL || 'http://127.0.0.1:19090';
const out = process.argv[2] || 'screenshot.jpeg';

const browser = await chromium.launch({ channel: 'chrome', headless: true });
const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, colorScheme: 'dark', locale: 'en-US' });
const page = await context.newPage();
// Demo data only: documentation IP (RFC 5737) and made-up disk sizes, so the
// screenshot shows nothing about the machine it was taken on.
await page.route('**/api/egress**', r => r.fulfill({ contentType: 'application/json', body: JSON.stringify({ ip: '203.0.113.42', city: 'Amsterdam', region: 'North Holland', country: 'NL', org: 'Example Networks', source: 'demo', checked_at: new Date().toISOString() }) }));
await page.route('**/api/disk', r => r.fulfill({ contentType: 'application/json', body: JSON.stringify({ bench_running: false, disks: [{ id: 'downloads', label: 'Downloads', total: 2000398934016, free: 1532608450560, used: 467790483456, low: false, paths: ['./downloads'] }] }) }));
const thumb = 'data:image/svg+xml;utf8,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360"><defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#7cc576"/><stop offset="0.55" stop-color="#2f7d4f"/><stop offset="1" stop-color="#14361f"/></linearGradient></defs><rect width="640" height="360" fill="url(#g)"/><circle cx="470" cy="110" r="46" fill="#f4e7a1" opacity="0.85"/><path d="M0 300 Q160 220 320 290 T640 270 V360 H0Z" fill="#1d4d2c"/><path d="M0 330 Q200 280 400 330 T640 320 V360 H0Z" fill="#163c22"/></svg>');
const formats = [
  { format_id: 'bv*+ba/b', label: 'Best quality', ext: 'mp4', has_video: true },
  { format_id: 'bv*[height<=1080]+ba/b[height<=1080]/bv*+ba/b', label: 'Up to 1080p', ext: 'mp4', has_video: true },
  { format_id: 'ba', label: 'Audio only', ext: 'mp3', has_video: false },
  { format_id: '401+140', label: '2160p · mp4 · av01 · 612 MB', ext: 'mp4', has_video: true },
  { format_id: '137+140', label: '1080p · mp4 · avc1 · 158 MB', ext: 'mp4', has_video: true },
  { format_id: '136+140', label: '720p · mp4 · avc1 · 74 MB', ext: 'mp4', has_video: true },
  { format_id: '140', label: 'm4a · 128 kbps · 9.6 MB', ext: 'm4a', has_video: false },
  { format_id: '251', label: 'webm · opus 160 kbps · 11 MB', ext: 'webm', has_video: false },
];
const sse = 'event: media\ndata: ' + JSON.stringify({ title: 'Big Buck Bunny 60fps 4K - Official Blender Foundation Short Film', uploader: 'Blender', duration: 635, thumbnail: thumb }) + '\n\nevent: formats\ndata: ' + JSON.stringify({ formats }) + '\n\n';
await page.route('**/api/formats', r => r.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body: sse }));

await page.goto(base + '/login');
await page.fill('input[name=username]', process.env.LEECHER_USER || 'demo');
await page.fill('input[name=password]', process.env.LEECHER_PASSWORD || '');
await page.press('input[name=password]', 'Enter');
await page.waitForURL(base + '/');
await page.waitForTimeout(1500);
await page.fill('#composer-input', 'https://www.youtube.com/watch?v=aqz-KE-bpKQ');
await page.press('#composer-input', 'Enter');
await page.waitForSelector('#preview:not([hidden])');
await page.waitForTimeout(1200);
await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
await page.mouse.move(1, 1);
await page.waitForTimeout(500);
await page.screenshot({ path: out, type: 'jpeg', quality: 88, fullPage: true });
console.log(JSON.stringify(await page.evaluate(() => ({ w: innerWidth, h: innerHeight, scroll: document.documentElement.scrollHeight }))));
await browser.close();
