const fs = require('node:fs');
const { chromium } = require('playwright');
const version = require('playwright/package.json').version;
const request = JSON.parse(fs.readFileSync('/request.json', 'utf8'));
const timer = setTimeout(() => { process.stdout.write(JSON.stringify({ok:false,error:'Browser operation timed out'})); process.exit(1); }, 90000);
(async () => {
  let browser, page;
  const result = {ok:false, playwright:version, steps:[]};
  try {
    if (version !== '1.63.0') throw new Error('Playwright package pin differs');
    browser = await chromium.launch({headless:true});
    result.browser = browser.version();
    page = await browser.newPage({viewport:{width:1024,height:640}});
    page.setDefaultTimeout(10000);
    await page.setContent('<main id="smoke">pending</main><script>document.querySelector("main").textContent = String(6 * 7)</script>');
    if (await page.locator('#smoke').textContent() !== '42') throw new Error('Chromium JavaScript smoke failed');
    if (request.operation !== 'prepare') {
      const input = request.input || {};
      const url = new URL(input.path || '/', request.config.base_url);
      if (url.origin !== new URL(request.config.base_url).origin) throw new Error('Check path must stay on the configured application origin');
      const response = await page.goto(url.href,{waitUntil:'domcontentloaded'});
      if (!response || !response.ok()) throw new Error('Application is not reachable with a successful HTTP response');
      result.title = await page.title();
      if (request.operation === 'execute') {
        if (!Array.isArray(input.steps) || input.steps.length === 0 || input.steps.length > 100) throw new Error('Browser check needs 1–100 configured steps');
        let assertions = 0;
        for (const step of input.steps) {
          if (Object.keys(step).length !== 1) throw new Error('Each browser step needs exactly one operation');
          if (step.fill) await page.locator(step.fill.selector).fill(String(step.fill.value));
          else if (step.click) await page.locator(step.click).click();
          else if (step.press) await page.locator(step.press.selector).press(step.press.key);
          else if (step.text) {
            assertions++;
            await page.waitForFunction(({selector,value}) => document.querySelector(selector)?.textContent?.trim() === value,
              {selector:step.text.selector,value:String(step.text.equals)});
          } else if (step.visible) {
            assertions++; await page.locator(step.visible).waitFor({state:'visible'});
          } else if (step.count) {
            assertions++;
            if (await page.locator(step.count.selector).count() !== step.count.equals) throw new Error('Element count assertion failed');
          } else if (step.evaluate) {
            assertions++;
            if (await page.evaluate(step.evaluate) !== true) throw new Error('Browser expression did not return true');
          } else throw new Error('Unknown browser step');
          result.steps.push({operation:Object.keys(step)[0],passed:true});
        }
        if (!assertions) throw new Error('Browser checks must contain an assertion');
      }
    }
    result.ok = true;
  } catch (error) { result.error = String(error.message).slice(0,4000); }
  if (page && request.operation === 'execute' && request.config.screenshot !== false) {
    try {
      const png = await page.screenshot({type:'png'});
      if (png.length <= 1024*1024) result.screenshot_png = png.toString('base64');
    } catch {}
  }
  if (browser) await browser.close();
  clearTimeout(timer);
  process.stdout.write(JSON.stringify(result));
})().catch(() => {clearTimeout(timer); process.stdout.write(JSON.stringify({ok:false,error:'Browser process failed'})); process.exitCode=1;});
