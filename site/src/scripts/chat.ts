type HistoryItem = { role: 'user' | 'assistant'; content: string };
type Citation = { id: string; title: string; url: string };
type Metadata = { commit: string; fingerprint: string };
type ChatAnswer = {
  version: 1; answer: string; citations: Citation[]; knowledge: Metadata;
  validation?: { valid: boolean; errors: string[] };
};
type Turnstile = {
  render(element: HTMLElement, options: Record<string, unknown>): string;
  reset(widget: string): void;
};
declare global { interface Window { turnstile?: Turnstile } }

const bytes = (text: string) => new TextEncoder().encode(text).length;
const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
const metadata = (value: unknown): value is Metadata => object(value)
  && typeof value.commit === 'string' && /^[a-f0-9]{40}$/.test(value.commit)
  && typeof value.fingerprint === 'string' && /^[a-f0-9]{64}$/.test(value.fingerprint);

function citation(value: unknown): value is Citation {
  if (!object(value) || typeof value.id !== 'string' || typeof value.title !== 'string'
      || typeof value.url !== 'string' || value.title.length > 300 || value.url.length > 2048) return false;
  try {
    const url = new URL(value.url);
    return url.origin === 'https://microsoft.github.io' && url.pathname.startsWith('/waza/')
      && !url.username && !url.password && !url.search;
  } catch { return false; }
}

function answer(value: unknown): value is ChatAnswer {
  if (!object(value) || value.version !== 1 || typeof value.answer !== 'string'
      || !value.answer.trim() || value.answer.length > 16000 || !metadata(value.knowledge)
      || !Array.isArray(value.citations) || value.citations.length > 12 || !value.citations.every(citation)) return false;
  if (value.validation !== undefined) {
    const result = value.validation;
    if (!object(result) || typeof result.valid !== 'boolean' || !Array.isArray(result.errors)
        || result.errors.length > 30 || !result.errors.every((error) => typeof error === 'string' && error.length <= 1000)) return false;
  }
  return true;
}

async function json(response: Response): Promise<unknown> {
  if (!response.body) throw new Error('The chat service returned an empty response.');
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let text = '';
  let size = 0;
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      size += part.value.byteLength;
      if (size > 65536) throw new Error('The chat service returned an oversized response.');
      text += decoder.decode(part.value, { stream: true });
    }
    text += decoder.decode();
  } finally {
    await reader.cancel();
  }
  try { return JSON.parse(text); } catch { throw new Error('The chat service returned an invalid response.'); }
}

let challengeScript: Promise<void> | undefined;
function loadChallenge() {
  if (window.turnstile) return Promise.resolve();
  if (!challengeScript) {
    challengeScript = new Promise<void>((resolve, reject) => {
      const script = document.createElement('script');
      const timeout = window.setTimeout(() => reject(new Error('Bot protection could not load. Please use documentation search.')), 15000);
      script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';
      script.async = true;
      script.onload = () => { clearTimeout(timeout); resolve(); };
      script.onerror = () => { clearTimeout(timeout); reject(new Error('Bot protection could not load. Please use documentation search.')); };
      document.head.append(script);
    });
  }
  return challengeScript;
}

export async function initializeChat(root: HTMLElement) {
  const required = <T extends HTMLElement>(selector: string) => {
    const element = root.querySelector<T>(selector);
    if (!element) throw new Error('Chat component configuration is incomplete.');
    return element;
  };
  const dialog = required<HTMLDialogElement>('[data-chat-dialog]');
  const form = required<HTMLFormElement>('[data-chat-form]');
  const question = required<HTMLTextAreaElement>('#chat-question');
  const yaml = required<HTMLTextAreaElement>('#chat-yaml');
  const kind = required<HTMLSelectElement>('#chat-document-kind');
  const status = required('[data-chat-status]');
  const knowledge = required('[data-chat-knowledge]');
  const messages = required<HTMLOListElement>('[data-chat-messages]');
  const send = required<HTMLButtonElement>('[data-chat-send]');
  const cancel = required<HTMLButtonElement>('[data-chat-cancel]');
  const challenge = required('[data-chat-challenge]');
  const api = root.dataset.apiUrl!;
  let controller: AbortController | undefined;
  let session: { token: string; expires: number } | undefined;
  let challengeToken = '';
  let widget: string | undefined;
  let expected: Metadata | undefined;
  let history: HistoryItem[] = [];
  let busy = false;
  const sessionValid = () => session && session.expires > Date.now() + 1000;
  const update = () => {
    send.disabled = busy || !expected || (!sessionValid() && !challengeToken);
    cancel.disabled = !busy;
    question.disabled = busy;
    yaml.disabled = busy;
    kind.disabled = busy;
  };
  const message = (label: string, content: string) => {
    const item = document.createElement('li');
    const heading = document.createElement('strong');
    heading.textContent = label;
    const text = document.createElement('pre');
    text.textContent = content;
    item.append(heading, text);
    messages.append(item);
    while (messages.children.length > 12) messages.firstElementChild?.remove();
    item.scrollIntoView({ block: 'nearest' });
    return item;
  };
  cancel.addEventListener('click', () => controller?.abort());
  dialog.addEventListener('close', () => controller?.abort());
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (busy || !expected) return;
    const input = question.value.trim();
    const documentText = yaml.value.trim();
    if (!input) { status.textContent = 'Enter a question first.'; return; }
    if (bytes(input) + bytes(documentText) > 12288) {
      status.textContent = 'Please shorten your question and YAML to 12 KiB or less.'; return;
    }
    if (!['eval', 'task', 'config'].includes(kind.value)) {
      status.textContent = 'Choose a supported document type.'; return;
    }
    const prior: HistoryItem[] = [];
    let inputSize = bytes(input) + bytes(documentText);
    for (const entry of history.slice(-6).reverse()) {
      if (inputSize + bytes(entry.content) > 12288) break;
      prior.unshift(entry);
      inputSize += bytes(entry.content);
    }
    busy = true;
    controller = new AbortController();
    const signal = controller.signal;
    const timeout = window.setTimeout(() => controller?.abort(), 45000);
    update();
    status.textContent = 'Checking the documentation...';
    try {
      if (!sessionValid()) {
        if (!challengeToken) throw new Error('Complete bot protection before sending.');
        const token = challengeToken;
        challengeToken = '';
        const response = await fetch(`${api}/session`, {
          method: 'POST', credentials: 'omit', cache: 'no-store', signal,
          headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ challengeToken: token }),
        });
        if (widget !== undefined) window.turnstile?.reset(widget);
        const value = await json(response);
        if (!response.ok) throw new Error(response.status === 429
          ? 'The session limit has been reached. Please use documentation search.'
          : 'Bot verification or session creation failed. Please try a new challenge.');
        if (!object(value) || value.version !== 1 || typeof value.sessionToken !== 'string'
            || !value.sessionToken || value.sessionToken.length > 4096
            || typeof value.expiresAt !== 'number' || !Number.isFinite(value.expiresAt)
            || value.expiresAt <= Date.now()) throw new Error('The chat service returned an invalid session.');
        session = { token: value.sessionToken, expires: value.expiresAt };
      }
      const response = await fetch(`${api}/chat`, {
        method: 'POST', credentials: 'omit', cache: 'no-store', signal,
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${session!.token}` },
        body: JSON.stringify({
          question: input, history: prior, requestId: crypto.randomUUID(),
          ...(documentText ? { document: { kind: kind.value, yaml: documentText } } : {}),
        }),
      });
      if (!response.ok) {
        if (response.status === 401) session = undefined;
        const errors: Record<number, string> = {
          401: 'Your session has expired. Complete a new bot challenge.',
          413: 'Please shorten your question or YAML.',
          429: 'The chat usage limit has been reached. Please use documentation search.',
          503: 'Chat is unavailable or its documentation is being updated. Please use search.',
          504: 'The chat service timed out. No automatic retry was made.',
        };
        throw new Error(errors[response.status] ?? 'The chat request failed. No automatic retry was made.');
      }
      const value = await json(response);
      if (!answer(value)) throw new Error('The chat service returned an invalid answer.');
      if (value.knowledge.fingerprint !== expected.fingerprint) {
        throw new Error('The assistant documentation needs updating. Please use documentation search.');
      }
      if (signal.aborted) throw new DOMException('Cancelled', 'AbortError');
      message('You', input);
      const item = message('Waza', value.answer);
      if (value.validation) {
        const result = document.createElement('pre');
        result.textContent = [
          value.validation.valid ? 'Schema valid (not proof of runtime correctness).' : 'Schema validation findings:',
          ...value.validation.errors,
        ].join('\n');
        item.append(result);
      }
      const sources = document.createElement('ul');
      for (const source of value.citations) {
        const row = document.createElement('li');
        const link = document.createElement('a');
        link.textContent = source.title;
        link.href = source.url;
        link.rel = 'noopener noreferrer';
        row.append(link);
        sources.append(row);
      }
      item.append(sources);
      const nextHistory: HistoryItem[] = [...prior,
        { role: 'user', content: input }, { role: 'assistant', content: value.answer }];
      history = nextHistory.slice(-6);
      question.value = '';
      yaml.value = '';
      status.textContent = 'Answer ready. Verify suggestions against the cited documentation.';
    } catch (error) {
      status.textContent = signal.aborted
        ? 'Request cancelled or timed out. No automatic retry was made.'
        : error instanceof Error ? error.message : 'Chat failed. Please use documentation search.';
      if (!sessionValid() && widget !== undefined) window.turnstile?.reset(widget);
    } finally {
      clearTimeout(timeout);
      controller = undefined;
      busy = false;
      update();
      if (dialog.open) question.focus();
    }
  });
  question.focus();
  status.textContent = 'Loading bot protection...';
  try {
    const response = await fetch(root.dataset.metaUrl!, {
      credentials: 'omit', cache: 'no-store', signal: AbortSignal.timeout(15000),
    });
    if (!response.ok) throw new Error('Documentation metadata is unavailable. Please use search.');
    const value = await json(response);
    if (!object(value) || value.version !== 1 || !metadata(value)) throw new Error('Documentation metadata is invalid. Please use search.');
    expected = value;
    knowledge.textContent = `Knowledge must match docs build ${value.commit.slice(0, 12)}.`;
    await loadChallenge();
    if (!window.turnstile) throw new Error('Bot protection is unavailable. Please use search.');
    widget = window.turnstile.render(challenge, {
      sitekey: root.dataset.sitekey!, action: 'waza_chat', theme: 'auto',
      callback: (token: string) => {
        challengeToken = token;
        if (!busy) status.textContent = 'Ready for your question.';
        update();
      },
      'expired-callback': () => { challengeToken = ''; status.textContent = 'Bot verification expired. Please complete a new challenge.'; update(); },
      'error-callback': () => { challengeToken = ''; status.textContent = 'Bot verification failed. Please use documentation search.'; update(); },
    });
  } catch (error) {
    status.textContent = error instanceof Error ? error.message : 'Chat is unavailable. Please use search.';
  }
  update();
}
