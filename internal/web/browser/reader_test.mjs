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

// Scrolled down before anything else, so the extract made below is not made
// from the very top of the page: right at the top there is nothing above
// the read point that could shift and drag the viewport with it, which
// would make check 6 pass trivially regardless of whether the scroll
// anchor in app.js actually works. A no-op on a short article that cannot
// scroll this far, which is fine — check 6 still holds, just less usefully.
await page.evaluate(() => window.scrollBy(0, 400));
await page.waitForTimeout(100);

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
page.on('response', async r => {
  if (r.url().includes('/extract')) responses.push({ status: r.status(), body: await r.text().catch(() => '') });
});

const scrollBeforeExtract = await page.evaluate(() => window.scrollY);

await page.click('#selection-toolbar button:has-text("Extract")');
await page.waitForTimeout(1200);

console.log('4. extract requests:', requests.length, 'status:', responses.map(r => r.status));
if (requests.length === 0) fail('clicking Extract sent no request');
if (responses[0] && responses[0].status !== 200) {
  console.log('   server said:', responses[0].body);
}
if (responses[0]?.status !== 200) fail('extract returned ' + responses[0]?.status + ', want 200');

const posted = requests[0].postData() || '';
console.log('   posted:', posted.slice(0, 90));
if (!posted.includes('start_block')) fail('the request carried no selection coordinates');

// 5. The article re-renders with the new highlight.
const marks = await page.locator('#article mark.extract').count();
console.log('5. highlights in the article:', marks);
if (marks === 0) fail('no highlight appeared after extracting');

// 6. The viewport must not jump. htmx swaps #article wholesale for the
//    outerHTML the extract response carries, and the fresh HTML settles at
//    a different height than the old one whenever an image the server
//    could not size (SVG, AVIF) or MathJax's async re-typeset changes
//    things above the read point — app.js anchors the scroll position on
//    the topmost visible block across exactly that swap to compensate.
//    Waited well past its 2.5s settling budget (plus MathJax's own typeset,
//    which can outlast that budget on a math-heavy article) before
//    comparing, and a little slack is allowed on top of that for a
//    straggler that finishes even later than that; the bug this guards
//    against was a jump of a screen or more, not a few pixels.
await page.waitForTimeout(1800);
const scrollAfterExtract = await page.evaluate(() => window.scrollY);
const scrollDrift = Math.abs(scrollAfterExtract - scrollBeforeExtract);
console.log('6. scroll position preserved across extract:',
            scrollBeforeExtract, '->', scrollAfterExtract, '(drift ' + scrollDrift + 'px)');
if (scrollDrift > 40) fail('extracting moved the viewport by ' + scrollDrift + 'px');

// 7. The toolbar goes away afterwards.
await page.waitForTimeout(300);
const stuck = await page.isVisible('#selection-toolbar');
console.log('7. toolbar hidden after extracting:', !stuck);
if (stuck) fail('the toolbar stayed on screen after use');

// 8. Deleting the extract just made. This exercises hx-delete + hx-confirm
//    together for the first time — a native confirm() dialog would otherwise
//    hang the page forever waiting for a click nothing will send, so the
//    dialog is auto-accepted before it appears.
//
// Done before any grading, which navigates away from this article — the
// "Extracts from this" list is rendered on a full page load rather than by the
// htmx swap that created the extract, so a reload is needed to show it, and
// that reload must still be of the article the extract belongs to.
page.on('dialog', d => d.accept());

await page.reload();
const extractLink = page.locator('.extracts a').first();
if (!(await extractLink.count())) fail('the new extract is not listed under "Extracts from this"');
await extractLink.click();
await page.waitForSelector('#article [data-b]');

const extractURL = page.url();
console.log('8. opened the extract at', extractURL);

const deleteRequests = [];
page.on('response', r => {
  if (r.request().method() === 'DELETE' && r.url().includes('/elements/')) deleteRequests.push(r.status());
});
await page.click('button.link-danger:has-text("delete")');
await page.waitForTimeout(1200);

console.log('   delete request status:', deleteRequests);
if (deleteRequests.length === 0) fail('clicking delete sent no DELETE request');
if (deleteRequests[0] !== 204 && deleteRequests[0] !== 303 && deleteRequests[0] !== 200) {
  fail('delete returned ' + deleteRequests[0]);
}
if (page.url() === extractURL) fail('deleting did not navigate away from the deleted extract');
console.log('   now at', page.url());
if (page.url() !== url) fail('deleting the extract did not return to its parent article');

// Its mark is gone from the article it came from.
const marksAfterDelete = await page.locator('#article mark.extract').count();
console.log('   highlights remaining in the parent:', marksAfterDelete);
if (marksAfterDelete !== 0) fail('the deleted extract\'s highlight is still shown');

// 9. The grading bar. Each button must carry the interval its grade produces,
//    and clicking one must actually navigate to the next element — the whole
//    bar is hx-post driven, so it fails the same silent way Extract did.
const labels = await page.locator('.grade-buttons button').allInnerTexts();
console.log('9. grade buttons:', JSON.stringify(labels));
if (labels.length < 4) fail('the grading bar is missing buttons');
if (!labels.some(l => /\d+(d|mo|y)/.test(l))) fail('no button shows an interval');

for (const group of ['Finished', 'Skip', 'Schedule']) {
  if (!(await page.locator('.grade-label', { hasText: group }).count())) {
    fail('no ' + group + ' group in the grading bar');
  }
}

const graded = [];
page.on('response', r => { if (r.url().includes('/grade')) graded.push(r.status()); });
await page.click('.grade-buttons button:has-text("Later")');
await page.waitForTimeout(1200);
console.log('10. bury: request status', graded, '-> now at', page.url());
if (graded.length === 0) fail('clicking a grade button sent no request');
if (!page.url().includes('/read/')) fail('grading did not move on to the next element');

await browser.close();
if (!process.exitCode) console.log('\nALL CHECKS PASSED');
