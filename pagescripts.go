package main

import "strings"

// JavaScript run in shared pages, ported from Eyebrows' agent tools. Each
// script is the body of an async function of args, whose value comes
// back as JSON. In Chrome they run in an isolated world (the page's DOM,
// but not its globals, so a page can't tamper with them, and doesn't see
// them); in Safari, AppleScript only reaches the page's own world.
//
// Elements found by readScript are remembered, by number, in
// window.__shoulder, for click and type to use.

// pagePrelude defines the shared helpers once per document.
const pagePrelude = `
if (!window.__shoulder) {
  const sh = { els: [] };
  Object.defineProperty(window, '__shoulder', { value: sh, enumerable: false, configurable: true });
  sh.visible = (el) => {
    if (!el || !el.getClientRects || el.getClientRects().length === 0) return false;
    const s = getComputedStyle(el);
    return s.visibility !== 'hidden' && s.display !== 'none' && parseFloat(s.opacity || '1') > 0.02;
  };
  sh.selector = (el) => {
    if (!(el instanceof Element)) return '';
    if (el.id && document.querySelectorAll('#' + CSS.escape(el.id)).length === 1) return '#' + CSS.escape(el.id);
    const parts = [];
    let node = el;
    while (node && node.nodeType === 1 && node !== document.documentElement) {
      if (node.id && document.querySelectorAll('#' + CSS.escape(node.id)).length === 1) { parts.unshift('#' + CSS.escape(node.id)); break; }
      let part = node.localName;
      const name = node.getAttribute('name');
      if (name && ['input','select','textarea','button','form'].includes(part)) part += '[name="' + name.replace(/"/g, '\\"') + '"]';
      const parent = node.parentElement;
      if (parent) {
        const same = [...parent.children].filter(c => c.localName === node.localName);
        if (same.length > 1) part += ':nth-of-type(' + (same.indexOf(node) + 1) + ')';
      }
      parts.unshift(part);
      node = parent;
    }
    return parts.join(' > ');
  };
  sh.interactive = (el) => {
    const tag = el.localName;
    if (['a','button','input','select','textarea','summary','option'].includes(tag)) {
      if (tag === 'a' && !el.hasAttribute('href') && !el.hasAttribute('onclick') && el.getAttribute('role') !== 'button') return false;
      if (tag === 'input' && el.type === 'hidden') return false;
      return true;
    }
    const role = el.getAttribute('role');
    if (role && ['button','link','checkbox','radio','tab','menuitem','menuitemcheckbox','menuitemradio','option','switch','combobox','textbox','searchbox','slider','treeitem'].includes(role)) return true;
    if (el.isContentEditable && (!el.parentElement || !el.parentElement.isContentEditable)) return true;
    if (el.hasAttribute('onclick') || el.hasAttribute('tabindex') && el.tabIndex >= 0 && tag !== 'div' && tag !== 'span') return true;
    return false;
  };
  sh.label = (el) => {
    const t = (s) => (s || '').replace(/\s+/g, ' ').trim();
    let l = t(el.getAttribute('aria-label')) || t(el.getAttribute('title'));
    if (!l && el.labels && el.labels.length) l = t(el.labels[0].innerText);
    if (!l && ['input','textarea'].includes(el.localName)) l = t(el.getAttribute('placeholder'));
    if (!l && el.type !== 'password') l = t(el.innerText || el.value || el.alt || '');
    if (!l) { const img = el.querySelector && el.querySelector('img[alt]'); if (img) l = t(img.alt); }
    return l.slice(0, 120);
  };
  sh.describe = (el, i) => {
    const tag = el.localName;
    const role = el.getAttribute('role');
    let kind = role || tag;
    if (tag === 'input') kind = 'input:' + (el.type || 'text');
    if (i === undefined) { const n = sh.els.indexOf(el); if (n >= 0) i = n + 1; }
    let s = '[' + (i === undefined ? '' : i + ' ') + kind;
    const label = sh.label(el);
    if (label) s += ' "' + label.replace(/"/g, "'") + '"';
    if (tag === 'a' && el.href && !el.href.startsWith('javascript:')) s += ' -> ' + el.href.slice(0, 200);
    if (['input','textarea','select'].includes(tag)) {
      if (el.type === 'checkbox' || el.type === 'radio') s += el.checked ? ' checked' : ' unchecked';
      else if (el.type !== 'password' && el.value) s += ' value="' + String(el.value).slice(0, 80) + '"';
      else if (el.type === 'password' && el.value) s += ' value=(hidden)';
      if (tag === 'select') s += ' options=' + JSON.stringify([...el.options].slice(0, 15).map(o => o.text.trim()));
    }
    if (el.disabled) s += ' disabled';
    if (el === document.activeElement) s += ' focused';
    return s + ']';
  };
  sh.resolve = (args) => {
    if (args.element !== undefined && args.element !== null) {
      const el = sh.els[Number(args.element) - 1];
      if (!el || !el.isConnected) throw new Error('element ' + args.element + ' is gone (the page changed); GET page again for fresh numbers');
      return el;
    }
    if (args.selector) {
      const el = document.querySelector(args.selector);
      if (!el) throw new Error('no element matches ' + args.selector);
      return el;
    }
    return null;
  };
}
const sh = window.__shoulder;
`

// readScript: the page as Markdown-ish text with numbered interactive
// elements. Args: start, max_chars, viewport_only.
const readScript = pagePrelude + `
sh.els = [];
const out = [];
let line = '';
const push = (s) => { line += s; };
const br = (n = 1) => { if (line.trim()) out.push(line.replace(/[ \t]+/g, ' ').trim()); line = ''; if (n > 1 && out.length && out[out.length-1] !== '') out.push(''); };
const vw = innerWidth, vh = innerHeight;
const inView = (el) => { const r = el.getBoundingClientRect(); return r.bottom > 0 && r.right > 0 && r.top < vh && r.left < vw; };
// Elements are numbered the same either way; viewport_only only leaves
// out what's off screen.
const show = (el) => !args.viewport_only || !el || !el.getBoundingClientRect || inView(el);
const skip = new Set(['script','style','noscript','template','svg','canvas','iframe','head','meta','link']);
const block = new Set(['p','div','section','article','main','header','footer','nav','aside','ul','ol','table','tr','form','fieldset','figure','blockquote','pre','dl','dt','dd','details','h1','h2','h3','h4','h5','h6','li','br','hr']);
const walk = (node) => {
  if (node.nodeType === 3) { const t = node.textContent; if (t && t.trim() && show(node.parentElement)) push(t); return; }
  if (node.nodeType !== 1) return;
  const el = node;
  const tag = el.localName;
  if (skip.has(tag)) return;
  if (el.getAttribute('aria-hidden') === 'true' && !el.querySelector('input,button,a,select,textarea')) return;
  if (!sh.visible(el) && tag !== 'option') { if (el.children.length === 0 || getComputedStyle(el).display === 'none') return; }
  const shown = show(el);
  if (sh.interactive(el)) {
    const i = sh.els.length;
    sh.els.push(el);
    if (shown) push(' ' + sh.describe(el, i + 1) + ' ');
    if (['a','button','summary','select','input','textarea'].includes(tag) || el.getAttribute('role') === 'button') return;
  }
  if (block.has(tag)) br(tag === 'p' || /^h\d$/.test(tag) || tag === 'section' ? 2 : 1);
  if (!shown) {}
  else if (/^h\d$/.test(tag)) push('#'.repeat(Number(tag[1])) + ' ');
  else if (tag === 'li') push('- ');
  else if (tag === 'img' && el.alt && el.alt.trim()) push('[image: ' + el.alt.trim().slice(0, 80) + '] ');
  else if (tag === 'td' || tag === 'th') push(' | ');
  const root = el.shadowRoot ? [...el.shadowRoot.childNodes] : [];
  for (const c of [...root, ...el.childNodes]) walk(c);
  if (block.has(tag)) br(tag === 'p' || /^h\d$/.test(tag) ? 2 : 1);
};
walk(document.body || document.documentElement);
br();
let text = out.join('\n').replace(/\n{3,}/g, '\n\n');
const total = text.length;
const start = Math.max(0, Number(args.start || 0));
const max = Math.max(500, Number(args.max_chars || 20000));
text = text.slice(start, start + max);
return {
  url: location.href, title: document.title,
  scroll: { x: Math.round(scrollX), y: Math.round(scrollY), height: document.documentElement.scrollHeight, viewport: [vw, vh] },
  elements: sh.els.length, total, start, truncated: start + text.length < total, text
};
`

// locateScript scrolls an element into view and returns its center (in
// viewport CSS pixels) and what's on top of it there.
const locateScript = pagePrelude + `
const el = sh.resolve(args);
if (!el) throw new Error('give element (a number from page) or selector');
el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
const r = el.getBoundingClientRect();
const x = r.left + r.width / 2, y = r.top + r.height / 2;
const top = document.elementFromPoint(x, y);
const covered = top && top !== el && !el.contains(top) && !top.contains(el);
return { x, y, width: r.width, height: r.height, description: sh.describe(el),
         covered: covered ? sh.describe(top) : null };
`

// clickScript clicks by script: for browsers without real input, and
// elements with no size.
const clickScript = pagePrelude + `
let el = sh.resolve(args);
if (!el && args.x !== undefined) el = document.elementFromPoint(args.x, args.y);
if (!el) throw new Error('nothing to click there');
el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
const r = el.getBoundingClientRect();
const opts = { bubbles: true, cancelable: true, composed: true, view: window, clientX: r.left + r.width / 2, clientY: r.top + r.height / 2, button: 0,
               metaKey: !!args.meta, ctrlKey: !!args.ctrl, shiftKey: !!args.shift, altKey: !!args.alt };
for (let n = 0; n < (args.count || 1); n++) {
  for (const t of ['pointerdown', 'mousedown']) el.dispatchEvent(t.startsWith('pointer') ? new PointerEvent(t, opts) : new MouseEvent(t, opts));
  if (el.focus) el.focus();
  for (const t of ['pointerup', 'mouseup']) el.dispatchEvent(t.startsWith('pointer') ? new PointerEvent(t, opts) : new MouseEvent(t, opts));
  el.click();
}
if (args.count === 2) el.dispatchEvent(new MouseEvent('dblclick', opts));
return sh.describe(el);
`

// typeScript focuses the element (or keeps the focused one) and, with
// clear, empties it. Unless prepare_only, it then inserts the text as
// typing would (input events fire); a <select> gets the option whose
// text or value matches.
const typeScript = pagePrelude + `
let el = sh.resolve(args) || document.activeElement;
if (!el || el === document.body || el === document.documentElement) throw new Error('no element to type into: give element or selector, or click a field first');
el.scrollIntoView({ block: 'center', behavior: 'instant' });
el.focus();
const isField = el.localName === 'input' || el.localName === 'textarea';
const setValue = (v) => { const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), 'value').set; setter.call(el, v); };
const report = () => {
  const v = isField ? (el.type === 'password' ? '(hidden)' : el.value) : el.localName === 'select' ? el.value : el.innerText;
  return { described: sh.describe(el), value: String(v).slice(0, 300), select: el.localName === 'select' };
};
if (el.localName === 'select') {
  if (args.prepare_only) return report();
  const want = String(args.text);
  const opt = [...el.options].find(o => o.text.trim() === want || o.value === want)
    || [...el.options].find(o => o.text.toLowerCase().includes(want.toLowerCase()));
  if (!opt) throw new Error('no option ' + want + ' (options: ' + [...el.options].map(o => o.text.trim()).join(', ').slice(0, 300) + ')');
  el.value = opt.value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return report();
}
if (args.clear) {
  if (isField) { el.select(); } else { const s = getSelection(); s.selectAllChildren(el); }
  if (!document.execCommand('delete')) {
    if (isField) { setValue(''); el.dispatchEvent(new Event('input', { bubbles: true })); }
    else el.textContent = '';
  }
} else if (isField && typeof el.selectionStart === 'number') {
  try { el.setSelectionRange(el.value.length, el.value.length); } catch (e) {}
} else if (el.isContentEditable) {
  const s = getSelection(); s.selectAllChildren(el); s.collapseToEnd();
}
if (args.prepare_only) return report();
const before = isField ? el.value : el.textContent;
const ok = document.execCommand('insertText', false, args.text);
const after = isField ? el.value : el.textContent;
if (!ok || after === before) {
  if (isField) setValue((args.clear ? '' : el.value) + args.text);
  else el.textContent = (args.clear ? '' : el.textContent) + args.text;
  el.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText', data: args.text }));
}
el.dispatchEvent(new Event('change', { bubbles: true }));
return report();
`

// valueScript reports the focused field after typing.
const valueScript = pagePrelude + `
const el = document.activeElement;
if (!el || el === document.body) return { described: 'the page', value: '' };
const isField = el.localName === 'input' || el.localName === 'textarea';
const v = isField ? (el.type === 'password' ? '(hidden)' : el.value) : el.innerText;
return { described: sh.describe(el), value: String(v || '').slice(0, 300) };
`

// keyScript presses a key by script, for browsers without real input:
// keydown/keyup to the focused element, Enter submitting its form.
const keyScript = `
const el = document.activeElement || document.body;
const init = { key: args.key, code: args.code, bubbles: true, cancelable: true, composed: true,
               shiftKey: !!args.shift, ctrlKey: !!args.ctrl, altKey: !!args.alt, metaKey: !!args.meta };
const go = el.dispatchEvent(new KeyboardEvent('keydown', init));
// Synthetic key events don't edit or activate anything: do what the key would.
const editable = el.localName === 'input' || el.localName === 'textarea' || el.isContentEditable;
const plain = !args.ctrl && !args.meta && !args.alt;
if (go && editable && plain) {
  if (args.key.length === 1) document.execCommand('insertText', false, args.key);
  else if (args.key === 'Backspace') document.execCommand('delete');
  else if (args.key === 'Delete') document.execCommand('forwardDelete');
  else if (args.key === 'Enter' && el.localName !== 'input') document.execCommand('insertLineBreak');
}
if (go && args.meta && args.key === 'a' && editable && el.select) el.select();
el.dispatchEvent(new KeyboardEvent('keyup', init));
if (go && args.key === 'Enter' && el.form && el.localName === 'input') { el.form.requestSubmit ? el.form.requestSubmit() : el.form.submit(); }
else if (go && (args.key === 'Enter' || args.key === ' ') && !editable && (el.localName === 'button' || el.localName === 'a' || el.getAttribute('role') === 'button')) el.click();
if (go && args.key === 'Tab') {
  const f = [...document.querySelectorAll('a[href],button,input,select,textarea,[tabindex]')].filter(e => e.tabIndex >= 0 && !e.disabled);
  const i = f.indexOf(el);
  const next = f[(i + (args.shift ? f.length - 1 : 1)) % f.length];
  if (next) next.focus();
}
return el.localName;
`

const scrollScript = pagePrelude + `
const el = sh.resolve(args);
if (el) { el.scrollIntoView({ block: 'center', behavior: 'instant' }); }
else if (args.to === 'top') scrollTo(0, 0);
else if (args.to === 'bottom') scrollTo(0, document.documentElement.scrollHeight);
else {
  const amount = Number(args.amount || innerHeight * 0.85);
  const dx = args.direction === 'left' ? -amount : args.direction === 'right' ? amount : 0;
  const dy = args.direction === 'up' ? -amount : args.direction === 'down' || !args.direction ? amount : 0;
  // The page may scroll an inner element instead of the document.
  const before = scrollY;
  scrollBy(dx, dy);
  if (scrollY === before && dy !== 0) {
    const c = [...document.querySelectorAll('*')].find(e => e.scrollHeight > e.clientHeight + 20 && /(auto|scroll)/.test(getComputedStyle(e).overflowY) && e.clientHeight > innerHeight * 0.5);
    if (c) c.scrollBy(dx, dy);
  }
}
return { x: Math.round(scrollX), y: Math.round(scrollY), height: document.documentElement.scrollHeight, viewport: [innerWidth, innerHeight] };
`

const htmlScript = `
const el = args.selector ? document.querySelector(args.selector) : document.documentElement;
if (!el) return null;
const html = el.outerHTML;
return { html: html.slice(args.start, args.start + args.max), total: html.length };
`

// checkScript is what wait polls for: text in the page, an element.
const checkScript = `
if (args.selector && !document.querySelector(args.selector)) return false;
if (args.text && !(document.body && document.body.innerText.includes(args.text))) return false;
if (args.gone && document.querySelector(args.gone)) return false;
return true;
`

// loadStateScript is the page's load state, for browsers that don't say.
const loadStateScript = `return { state: document.readyState, url: location.href, title: document.title };`

// viewportScript: the viewport in CSS pixels, for screenshots.
const viewportScript = `return { width: innerWidth, height: innerHeight, dpr: devicePixelRatio, x: scrollX, y: scrollY };`

// userCode wraps the agent's code for js: the body of an async function,
// run in the page's own world; the value comes back as JSON (elements as
// their HTML). A lone expression has its value returned.
func userCode(code string, expression bool) string {
	body := code
	if expression {
		body = "return (\n" + code + "\n);"
	}
	return `(async () => {
const __value = await (async () => {
` + body + `
})();
const __seen = new WeakSet();
const __clean = (v, depth) => {
  if (v === undefined) return null;
  if (v === null || typeof v === 'number' || typeof v === 'boolean' || typeof v === 'string') return v;
  if (typeof v === 'bigint') return String(v);
  if (typeof v === 'function') return '[function ' + (v.name || 'anonymous') + ']';
  if (v instanceof Element) return v.outerHTML.length > 2000 ? v.outerHTML.slice(0, 2000) + '…' : v.outerHTML;
  if (v instanceof Node) return v.textContent;
  if (depth > 6) return '[…]';
  if (typeof v === 'object') {
    if (__seen.has(v)) return '[circular]';
    __seen.add(v);
    if (Array.isArray(v) || v instanceof NodeList || v instanceof HTMLCollection) return [...v].slice(0, 500).map(x => __clean(x, depth + 1));
    if (v instanceof Map) return Object.fromEntries([...v].slice(0, 500).map(([k, x]) => [String(k), __clean(x, depth + 1)]));
    if (v instanceof Set) return [...v].slice(0, 500).map(x => __clean(x, depth + 1));
    if (v instanceof Error) return { error: String(v) };
    const o = {};
    for (const k of Object.keys(v).slice(0, 500)) { try { o[k] = __clean(v[k], depth + 1); } catch (e) { o[k] = '[unreadable]'; } }
    return o;
  }
  return String(v);
};
return JSON.stringify(__clean(__value, 0));
})()`
}

// isExpression guesses whether the agent's code is a lone expression,
// whose value it wants, rather than a function body.
func isExpression(code string) bool {
	c := strings.TrimSpace(code)
	for _, p := range []string{"const ", "let ", "var ", "if ", "if(", "for ", "for(", "while", "try", "function ", "throw "} {
		if len(c) >= len(p) && c[:len(p)] == p {
			return false
		}
	}
	return !strings.Contains(c, "return") && !strings.ContainsAny(c, ";\n")
}
