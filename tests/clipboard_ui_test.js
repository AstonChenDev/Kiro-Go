const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const appSource = fs.readFileSync(path.join(__dirname, '..', 'web', 'app.js'), 'utf8');
const copyFunctionMatch = appSource.match(/  async function copyText\(input\) \{[\s\S]*?\n  \}\n  function isClipboardError/);

if (!copyFunctionMatch) {
  throw new Error('copyText implementation was not found in web/app.js');
}

const copyFunctionSource = copyFunctionMatch[0].replace(/\n  function isClipboardError$/, '');

function createHarness({ writeText, execCommand = () => true } = {}) {
  const calls = {
    appended: 0,
    removed: 0,
    focused: 0,
    selected: 0,
    selectionRange: null,
    restoredFocus: 0,
    exec: 0,
  };
  const textarea = {
    value: '',
    readOnly: false,
    className: '',
    tabIndex: 0,
    style: {},
    setAttribute() {},
    focus() { calls.focused += 1; },
    select() { calls.selected += 1; },
    setSelectionRange(start, end) { calls.selectionRange = [start, end]; },
  };
  const activeElement = {
    focus() { calls.restoredFocus += 1; },
  };
  const document = {
    activeElement,
    body: {
      appendChild(node) {
        assert.equal(node, textarea);
        calls.appended += 1;
      },
      removeChild(node) {
        assert.equal(node, textarea);
        calls.removed += 1;
      },
    },
    createElement(tag) {
      assert.equal(tag, 'textarea');
      return textarea;
    },
    execCommand(command) {
      assert.equal(command, 'copy');
      calls.exec += 1;
      return execCommand();
    },
  };
  const navigator = writeText ? { clipboard: { writeText } } : {};
  const context = vm.createContext({ document, navigator, ClipboardItem: undefined });
  vm.runInContext(`${copyFunctionSource}\nglobalThis.copyText = copyText;`, context);
  return { calls, copyText: context.copyText, textarea };
}

test('uses the modern clipboard API when it succeeds', async () => {
  let written = '';
  const harness = createHarness({
    writeText: async text => { written = text; },
  });

  assert.equal(await harness.copyText('actual-key'), true);
  assert.equal(written, 'actual-key');
  assert.equal(harness.calls.exec, 0);
});

test('focuses and selects the textarea before using the HTTP fallback', async () => {
  const harness = createHarness({
    writeText: async () => { throw new Error('permission denied'); },
  });

  assert.equal(await harness.copyText('fallback-key'), true);
  assert.equal(harness.textarea.value, 'fallback-key');
  assert.equal(harness.calls.focused, 1);
  assert.equal(harness.calls.selected, 1);
  assert.deepEqual(harness.calls.selectionRange, [0, 12]);
  assert.equal(harness.calls.exec, 1);
  assert.equal(harness.calls.appended, 1);
  assert.equal(harness.calls.removed, 1);
  assert.equal(harness.calls.restoredFocus, 1);
});

test('rejects instead of reporting success when every copy method fails', async () => {
  const harness = createHarness({ execCommand: () => false });

  await assert.rejects(harness.copyText('not-copied'), error => {
    assert.equal(error.name, 'ClipboardError');
    return true;
  });
  assert.equal(harness.calls.removed, 1);
  assert.equal(harness.calls.restoredFocus, 1);
});
