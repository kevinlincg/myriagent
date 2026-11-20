// 新增帳戶對話框「使用現有的設定資料夾」的純規則（宿主：Accounts/ExistingFolderRules.cs、AccountManager.AddExisting）。
// 使用者在終端機以 CLAUDE_CONFIG_DIR=~/.claude-work 之類分開的帳戶，直接把那個資料夾加成帳戶：登入、Skills、外掛、記憶都是同一份。
// 沒有 import、不碰 DOM（tools/ui-probe/existingfolderrules.test.mjs 以 data: URL 匯入測試）。路徑與帳戶名稱都是不可信的字串，畫面一律以 textContent 顯示。

const str = (v) => (typeof v === 'string' && v ? v : null);
/** 比較用的路徑：去掉結尾的分隔符號、不分大小寫（同宿主 ExistingFolderRules.SameFolder） */
const key = (p) => String(p).replace(/[\\/]+$/, '').toLowerCase();

/**
 * 這種 CLI 可以直接加成帳戶的資料夾：宿主 init 的 providers[].existingFolders，去掉已經有帳戶在用的
 * （init 之後才加入的帳戶，宿主不會重送這份清單）與重複的。回 [{ path, display }]；info 是 providerInfo(key)
 */
export function folderChoices(info, accounts) {
  const list = info && Array.isArray(info.existingFolders) ? info.existingFolders : [];
  const used = new Set((Array.isArray(accounts) ? accounts : [])
    .filter((a) => a && a.provider === info.key && a.remoteOnly !== true && str(a.configDirectory))
    .map((a) => key(a.configDirectory)));
  const out = [];
  for (const f of list) {
    const path = f && str(f.path);
    if (!path || used.has(key(path)) || out.some((o) => key(o.path) === key(path))) continue;
    out.push({ path, display: str(f.display) || path });
  }
  return out;
}

const PREFIX = { Claude: '.claude', Codex: '.codex', Grok: '.grok' };

/**
 * 現有資料夾的預設帳戶名稱：~/.claude-work → 「Claude work」。資料夾名稱看不出來（不是 .claude 開頭、後面沒有字）回 null，
 * 用一般的預設名稱（同宿主 ExistingFolderRules.DefaultLabel；同名的帳戶已經有了由宿主改用編號）
 */
export function folderLabel(provider, path) {
  const prefix = PREFIX[provider];
  const text = str(path);
  if (!prefix || !text) return null;
  const name = text.trim().replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '';
  if (!name.toLowerCase().startsWith(prefix)) return null;
  const next = name.charAt(prefix.length);
  if (next && !'-_.'.includes(next)) return null;
  const rest = name.slice(prefix.length).replace(/^[-_. ]+|[-_. ]+$/g, '');
  return rest ? (provider + ' ' + rest).slice(0, 40) : null;
}
