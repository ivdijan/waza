import { expect, test, type Page } from '@playwright/test';

const knowledge = { commit: 'a'.repeat(40), fingerprint: 'b'.repeat(64) };
const validAnswer = {
  version: 1, answer: 'Use waza run with an eval specification.',
  citations: [{ id: 'quick-start', title: 'Quick Start', url: 'https://microsoft.github.io/waza/quick-start/' }],
  knowledge,
};

async function setup(page: Page, options: { noChallenge?: boolean; missingMetadata?: boolean } = {}) {
  await page.route('**/chat-meta.json', (route) => route.fulfill(options.missingMetadata
    ? { status: 503, body: 'unavailable' } : { json: { version: 1, ...knowledge } }));
  await page.route('https://waza-chat.example/api/session', (route) => route.fulfill({
    json: { version: 1, sessionToken: 'test-session', expiresAt: Date.now() + 600000 },
  }));
  await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({ json: validAnswer }));
  if (!options.noChallenge) {
    await page.addInitScript(() => {
      let verified: ((token: string) => void) | undefined;
      Object.assign(window, { turnstile: {
        render: (_element: HTMLElement, config: { callback: (token: string) => void }) => {
          verified = config.callback;
          config.callback('test-challenge');
          return 'test-widget';
        },
        reset: () => verified?.('fresh-test-challenge'),
      } });
    });
  }
  await page.goto('/waza/quick-start/');
  await page.getByRole('button', { name: 'Ask Waza', exact: true }).click();
}

async function send(page: Page, question = 'How do I run an eval?') {
  await page.getByLabel('Question or error message').fill(question);
  await page.getByRole('button', { name: 'Send', exact: true }).click();
}

test('lazy loads, exchanges challenge once and sends bounded typed conversation', async ({ page }) => {
  const requests: unknown[] = [];
  let sessions = 0;
  page.on('request', (request) => {
    if (request.url().endsWith('/api/chat')) requests.push(request.postDataJSON());
    if (request.url().endsWith('/api/session')) sessions++;
  });
  await setup(page);
  await send(page);
  await expect(page.getByRole('status')).toContainText('Answer ready');
  await expect(page.locator('[data-chat-messages]').getByRole('link', { name: 'Quick Start', exact: true })).toHaveAttribute('href', validAnswer.citations[0].url);
  expect(requests[0]).toMatchObject({ question: 'How do I run an eval?', history: [] });
  await send(page, 'And how do I read the results?');
  await expect(page.getByRole('status')).toContainText('Answer ready');
  expect(requests[1]).toMatchObject({ history: [
    { role: 'user', content: 'How do I run an eval?' },
    { role: 'assistant', content: validAnswer.answer },
  ] });
  expect(sessions).toBe(1);
  expect(await page.evaluate(() => Object.keys(localStorage).filter((key) => key !== 'starlight-theme'))).toEqual([]);
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('test-session');
});

test('renders HTML and code as inert text rather than executable Markdown', async ({ page }) => {
  await setup(page);
  await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({
    json: { ...validAnswer, answer: '<img src=x onerror="window.pwned=1"> [click](javascript:alert(1))' },
  }));
  await send(page);
  await expect(page.locator('[data-chat-messages]')).toContainText('<img src=x');
  await expect(page.locator('[data-chat-messages] img')).toHaveCount(0);
  await expect(page.locator('[data-chat-messages] a')).toHaveCount(1);
});

for (const [label, result] of [
  ['unsafe citation', { ...validAnswer, citations: [{ id: 'x', title: 'Unsafe', url: 'javascript:alert(1)' }] }],
  ['wrong knowledge snapshot', { ...validAnswer, knowledge: { ...knowledge, fingerprint: 'c'.repeat(64) } }],
  ['invalid structure', { answer: 'unvalidated' }],
] as const) {
  test(`rejects ${label} without displaying an answer`, async ({ page }) => {
    await setup(page);
    await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({ json: result }));
    await send(page);
    await expect(page.getByRole('status')).toContainText(/invalid answer|needs updating/);
    await expect(page.locator('[data-chat-messages] > li')).toHaveCount(0);
  });
}

for (const status of [401, 413, 429, 503, 504]) {
  test(`handles ${status} explicitly without retrying or echoing backend internals`, async ({ page }) => {
    await setup(page);
    let calls = 0;
    await page.route('https://waza-chat.example/api/chat', (route) => {
      calls++;
      return route.fulfill({ status, json: { error: { message: 'secret-canary-never-display' } } });
    });
    await send(page);
    await expect(page.getByRole('status')).toContainText(/expired|shorten|limit|unavailable|timed out/);
    expect(calls).toBe(1);
    await expect(page.locator('[data-chat-dialog]')).not.toContainText('secret-canary');
  });
}

test('YAML uses the explicit document type and validation does not imply runtime success', async ({ page }) => {
  await setup(page);
  await page.locator('summary').filter({ hasText: 'Check pasted YAML' }).click();
  await page.getByLabel('Document type').selectOption('task');
  await page.getByLabel('YAML without credentials').fill('id: example');
  await page.route('https://waza-chat.example/api/chat', async (route) => {
    expect(route.request().postDataJSON().document).toEqual({ kind: 'task', yaml: 'id: example' });
    await route.fulfill({ json: { ...validAnswer, validation: { valid: true, errors: [] } } });
  });
  await send(page);
  await expect(page.locator('[data-chat-messages]')).toContainText('not proof of runtime correctness');
});

test('unavailable metadata prevents challenge session or model calls', async ({ page }) => {
  await setup(page, { missingMetadata: true });
  await expect(page.getByRole('status')).toContainText('metadata is unavailable');
  await expect(page.getByRole('button', { name: 'Send', exact: true })).toBeDisabled();
});

test('unavailable challenge fails closed', async ({ page }) => {
  await page.route('https://challenges.cloudflare.com/**', (route) => route.abort());
  await setup(page, { noChallenge: true });
  await expect(page.getByRole('status')).toContainText('Bot protection could not load');
  await expect(page.getByRole('button', { name: 'Send', exact: true })).toBeDisabled();
});

test('cancellation does not display a late answer or automatically retry', async ({ page }) => {
  await setup(page);
  let calls = 0;
  await page.route('https://waza-chat.example/api/chat', async (route) => {
    calls++;
    await new Promise((resolve) => setTimeout(resolve, 500));
    await route.fulfill({ json: validAnswer });
  });
  await send(page);
  await expect.poll(() => calls).toBe(1);
  await page.getByRole('button', { name: 'Cancel request', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('cancelled');
  await expect(page.locator('[data-chat-messages] > li')).toHaveCount(0);
  expect(calls).toBe(1);
});

test('mobile dialog is keyboard accessible and Escape returns focus to launcher', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await setup(page);
  await expect(page.getByRole('dialog')).toBeVisible();
  await expect(page.getByLabel('Question or error message')).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).not.toBeVisible();
  await expect(page.getByRole('button', { name: 'Ask Waza', exact: true })).toBeFocused();
});

test('input byte budget rejects excessive Unicode and YAML before contacting the service', async ({ page }) => {
  await setup(page);
  let calls = 0;
  page.on('request', (request) => { if (request.url().startsWith('https://waza-chat.example/')) calls++; });
  await page.locator('summary').filter({ hasText: 'Check pasted YAML' }).click();
  await page.getByLabel('YAML without credentials').fill('a'.repeat(8192));
  await send(page, 'é'.repeat(4096));
  await expect(page.getByRole('status')).toContainText('shorten');
  expect(calls).toBe(0);
});

test('expired session response never authorizes a model request', async ({ page }) => {
  await setup(page);
  let calls = 0;
  page.on('request', (request) => { if (request.url().endsWith('/api/chat')) calls++; });
  await page.route('https://waza-chat.example/api/session', (route) => route.fulfill({
    json: { version: 1, sessionToken: 'expired', expiresAt: Date.now() - 1000 },
  }));
  await send(page);
  await expect(page.getByRole('status')).toContainText('invalid session');
  expect(calls).toBe(0);
});

test('failed server-side challenge does not expose internals or call the model', async ({ page }) => {
  await setup(page);
  let calls = 0;
  page.on('request', (request) => { if (request.url().endsWith('/api/chat')) calls++; });
  await page.route('https://waza-chat.example/api/session', (route) => route.fulfill({
    status: 403, json: { error: { message: 'challenge-secret-canary' } },
  }));
  await send(page);
  await expect(page.getByRole('status')).toContainText('session creation failed');
  await expect(page.locator('[data-chat-dialog]')).not.toContainText('challenge-secret-canary');
  expect(calls).toBe(0);
});

test('oversized and malformed service bodies are not rendered', async ({ page }) => {
  await setup(page);
  await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({ body: 'x'.repeat(65537) }));
  await send(page);
  await expect(page.getByRole('status')).toContainText('oversized response');
  await expect(page.locator('[data-chat-messages] > li')).toHaveCount(0);
  await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({ body: '{broken' }));
  await send(page);
  await expect(page.getByRole('status')).toContainText('invalid response');
});

test('no-evidence answer is explicit and works in dark mode', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' });
  await setup(page);
  await page.route('https://waza-chat.example/api/chat', (route) => route.fulfill({
    json: { ...validAnswer, answer: 'I could not find supporting documentation for that question.', citations: [] },
  }));
  await send(page);
  await expect(page.locator('[data-chat-messages]')).toContainText('could not find supporting documentation');
  await expect(page.locator('[data-chat-messages] a')).toHaveCount(0);
});
