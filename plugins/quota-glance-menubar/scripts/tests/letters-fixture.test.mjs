import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { build, FIXTURE } from '../letters-fixture.mjs';

const committed = JSON.parse(readFileSync(FIXTURE, 'utf8'));
const letters = name => Object.fromEntries(committed.scenarios.find(s => s.name === name).rows.map(r => [`${r.providerID}/${r.rowID}`, r.letter]));

test('the committed letter fixture is the generator\'s current output', () => {
  assert.deepEqual(committed, build(), 'Run: node scripts/letters-fixture.mjs');
});

test('the fixture carries the board\'s Rules table', () => {
  const board = letters('board');
  assert.equal(board['claude/session'], 'S');
  assert.equal(board['claude/weekly'], 'W');
  assert.equal(board['claude/weekly_fable'], 'F');
  assert.equal(board['xai/credits'], 'C');
  assert.equal(board['codex/model_session:spark'], 'Ss');
  assert.equal(board['codex/model_weekly:spark'], 'Ws');
  assert.equal(board['claude/model_weekly:sonnet'], 'Ws');
  assert.equal(board['xai/raw:xai:product/BUILD'], 'P');
  const shared = letters('shared model initial');
  assert.equal(shared['codex/model_weekly:spark'], 'Wsp');
  assert.equal(shared['codex/model_weekly:sage'], 'Wsa');
  assert.equal(shared['codex/model_session:spark'], 'Ss');
});
