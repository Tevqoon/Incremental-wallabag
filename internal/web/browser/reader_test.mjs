// Browser test for the reader's selection flow.
//
// This exists because the Go tests structurally cannot cover it. They drive the
// handlers directly, so they verify that a correct request produces a correct
// extract — and say nothing about whether the browser ever sends one. Three
// bugs lived in exactly that gap, and the whole feature was unusable in a
// browser while every Go test passed:
//
//   1. `hidden` was overridden by an author-level `display: flex`, so the
//      toolbar never went away.
//   2. mousedown collapsed the selection before htmx read it, and the
//      activeElement guard meant to prevent that does not work in Safari,
//      which never focuses a clicked button.
//   3. hx-vals="js:fn()" is wrapped by htmx into "{fn()}", a syntax error it
//      swallows — so clicking Extract issued no request at all.
//
// Run against a running instance:
//   npm install playwright && npx playwright install chromium
//   BASE=http://127.0.0.1:8080 node internal/web/browser/reader_test.mjs
import { chromium } from 'playwright';

const BASE = process.env.BASE || 'http://127.0.0.1:8080';
const fail = (m) => { console.log('FAIL: ' + m); process.exitCode = 1; };

const browser = await chromium.launch();
const page = await browser.newPage();

// Pick an article that is in the queue and has its body.
const res = await page.goto(BASE + '/next');
const url = page.url();
console.log('reading:', url);

await page.waitForSelector('#article [data-b]');

// 1. The toolbar must start hidden. This is the bug from the screenshot:
//    `hidden` loses to an author-level `display: flex`.
const visibleAtRest = await page.isVisible('#selection-toolbar');
console.log('1. toolbar hidden at rest:', !visibleAtRest);
if (visibleAtRest) fail('toolbar is visible before anything is selected');

// 2. Selecting text must reveal it.
await page.evaluate(() => {
  const block = document.querySelector('#article p[data-b]');
  const text = block.firstChild;
  const range = document.createRange();
  range.setStart(text, 0);
  range.setEnd(text, Math.min(25, text.length));
  const sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);
  document.dispatchEvent(new Event('selectionchange'));
});
await page.waitForTimeout(200);
const visibleOnSelect = await page.isVisible('#selection-toolbar');
console.log('2. toolbar shown on selection:', visibleOnSelect);
if (!visibleOnSelect) fail('toolbar did not appear when text was selected');

// 3. The payload must survive the click. Previously the mousedown collapsed
//    the selection and the activeElement guard failed in WebKit.
const before = await page.evaluate(() => window.selectionPayload());
console.log('3. captured payload:', JSON.stringify(before));
if (before.quote === undefined) fail('nothing was captured from the selection');

const requests = [];
page.on('request', r => { if (r.url().includes('/extract')) requests.push(r); });
const responses = [];
page.on('response', r => { if (r.url().includes('/extract')) responses.push(r.status()); });

await page.click('#selection-toolbar button:has-text("Extract")');
await page.waitForTimeout(1200);

console.log('4. extract requests:', requests.length, 'status:', responses);
if (requests.length === 0) fail('clicking Extract sent no request');
if (responses[0] !== 200) fail('extract returned ' + responses[0] + ', want 200');

const posted = requests[0].postData() || '';
console.log('   posted:', posted.slice(0, 90));
if (!posted.includes('start_block')) fail('the request carried no selection coordinates');

// 5. The article re-renders with the new highlight.
const marks = await page.locator('#article mark.extract').count();
console.log('5. highlights in the article:', marks);
if (marks === 0) fail('no highlight appeared after extracting');

// 6. The toolbar goes away afterwards.
await page.waitForTimeout(300);
const stuck = await page.isVisible('#selection-toolbar');
console.log('6. toolbar hidden after extracting:', !stuck);
if (stuck) fail('the toolbar stayed on screen after use');

await browser.close();
if (!process.exitCode) console.log('\nALL CHECKS PASSED');
