import { expect, test } from '@playwright/test';

test('chat is absent by default and public corpus metadata contains no private configuration', async ({ page, request }) => {
  await page.goto('/waza/quick-start/');
  await expect(page.getByRole('button', { name: 'Ask Waza', exact: true })).toHaveCount(0);
  const response = await request.get('/waza/chat-meta.json');
  expect(response.ok()).toBeTruthy();
  const value = await response.json();
  expect(Object.keys(value).sort()).toEqual(['commit', 'fingerprint', 'version']);
  expect(value.version).toBe(1);
  expect(value.commit).toMatch(/^[a-f0-9]{40}$/);
  expect(value.fingerprint).toMatch(/^[a-f0-9]{64}$/);
});
