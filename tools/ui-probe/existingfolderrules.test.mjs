// 新增帳戶對話框「使用現有的設定資料夾」的純規則（src/Myriagent/Web/existingfolderrules.js）：列出哪些資料夾、預設的帳戶名稱，
// 以及這個模組沒有 import、沒有 HTML 寫入點。規則模組沒有 import，以 data: URL 匯入（同 sendrules.test.mjs）。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(here, '..', '..', 'src', 'Myriagent', 'Web');
const source = readFileSync(path.join(webDir, 'existingfolderrules.js'), 'utf8');
const R = await import('data:text/javascript;base64,' + Buffer.from(source + '\n// existingfolderrules.test\n', 'utf8').toString('base64'));

const claude = (existingFolders) => ({ key: 'Claude', name: 'Claude Code', isolatedAccounts: true, existingFolders });

test('folderChoices：照宿主給的順序，沒有 display 的用完整路徑', () => {
  const info = claude([{ path: '/Users/u/.claude-alt', display: '~/.claude-alt' }, { path: '/Users/u/.claude-work' }]);
  assert.deepEqual(R.folderChoices(info, []), [
    { path: '/Users/u/.claude-alt', display: '~/.claude-alt' },
    { path: '/Users/u/.claude-work', display: '/Users/u/.claude-work' },
  ]);
});

test('folderChoices：已經有帳戶在用的不列（不分大小寫、不管結尾的斜線），別種 CLI 與只在其他電腦的帳戶不算', () => {
  const info = claude([
    { path: '/Users/u/.claude-alt', display: '~/.claude-alt' },
    { path: '/Users/u/.claude-work', display: '~/.claude-work' },
    { path: '/Users/u/.claude-p', display: '~/.claude-p' },
  ]);
  const accounts = [
    { provider: 'Claude', configDirectory: '/Users/U/.Claude-Alt/' },
    { provider: 'Codex', configDirectory: '/Users/u/.claude-work' },
    { provider: 'Claude', configDirectory: '/Users/u/.claude-p', remoteOnly: true },
    { provider: 'Claude', configDirectory: null },
    null,
  ];
  assert.deepEqual(R.folderChoices(info, accounts).map((f) => f.path), ['/Users/u/.claude-work', '/Users/u/.claude-p']);
});

test('folderChoices：Windows 的路徑也一樣比', () => {
  const info = claude([{ path: 'C:\\Users\\U\\.claude-work', display: '~\\.claude-work' }]);
  assert.deepEqual(R.folderChoices(info, [{ provider: 'Claude', configDirectory: 'c:\\users\\u\\.claude-work\\' }]), []);
  assert.equal(R.folderChoices(info, [{ provider: 'Claude', configDirectory: 'C:\\Users\\U\\.claude' }]).length, 1);
});

test('folderChoices：清單不是陣列、項目不合格、重複的都略過', () => {
  assert.deepEqual(R.folderChoices(claude(undefined), []), []);
  assert.deepEqual(R.folderChoices(claude('x'), []), []);
  assert.deepEqual(R.folderChoices(null, []), []);
  assert.deepEqual(R.folderChoices({ key: 'Claude' }, null), []);
  const info = claude([null, {}, { path: '' }, { path: 7 }, { path: '/a/.claude-x' }, { path: '/A/.claude-x/', display: '重複' }]);
  assert.deepEqual(R.folderChoices(info, 'x'), [{ path: '/a/.claude-x', display: '/a/.claude-x' }]);
});

test('folderLabel：資料夾名稱後面那段字當帳戶名稱', () => {
  assert.equal(R.folderLabel('Claude', '/Users/u/.claude-work'), 'Claude work');
  assert.equal(R.folderLabel('Claude', '/Users/u/.claude-work/'), 'Claude work');
  assert.equal(R.folderLabel('Claude', '  ~/.claude_alt  '), 'Claude alt');
  assert.equal(R.folderLabel('Claude', 'C:\\Users\\U\\.Claude-Personal\\'), 'Claude Personal');
  assert.equal(R.folderLabel('Codex', '~/.codex.old'), 'Codex old');
  assert.equal(R.folderLabel('Grok', '~/.grok-2'), 'Grok 2');
  assert.equal(R.folderLabel('Claude', '~/.claude-' + 'a'.repeat(60)).length, 40);
});

test('folderLabel：看不出來的回 null（用一般的預設名稱）', () => {
  assert.equal(R.folderLabel('Claude', '~/.claude'), null);
  assert.equal(R.folderLabel('Claude', '~/.claude-'), null);
  assert.equal(R.folderLabel('Claude', '~/.claudex'), null);
  assert.equal(R.folderLabel('Claude', '~/profiles/work'), null);
  assert.equal(R.folderLabel('Claude', '~/.codex-work'), null);
  assert.equal(R.folderLabel('Antigravity', '~/.gemini-work'), null);
  assert.equal(R.folderLabel('Claude', ''), null);
  assert.equal(R.folderLabel('Claude', null), null);
  assert.equal(R.folderLabel(undefined, '~/.claude-work'), null);
});

test('模組沒有 import、不碰 DOM、沒有 HTML 寫入點', () => {
  assert.equal(/^\s*import\s/m.test(source), false);
  assert.equal(/\bdocument\b|\bwindow\b|innerHTML|insertAdjacentHTML/.test(source), false);
});

test('dashboard.js：帶 directory 的 accountAdd 只在填了資料夾時送，現有資料夾的帳戶移除時不送 deleteData', () => {
  const dash = readFileSync(path.join(webDir, 'dashboard.js'), 'utf8');
  assert.match(dash, /import \{ folderChoices, folderLabel \} from '\.\/existingfolderrules\.js';/);
  assert.match(dash, /if \(dir\) \{[\s\S]{0,300}cmd: 'accountAdd', provider, label: name\.value\.trim\(\), color, directory: dir/);
  assert.match(dash, /deleteData: !!\(r\.checked && !a\.isSystemDefault && !a\.existingFolder\)/);
  assert.match(dash, /sharedCustomizations && !a\.isSystemDefault && !a\.existingFolder/);
});
