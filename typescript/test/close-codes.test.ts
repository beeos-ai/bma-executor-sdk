// Only the contract's stop codes end the client: 4003 unsupported protocol and
// 4004 replaced. A restarting server (4002 runtime ended, 1001/1012/1013,
// 4005 drained, 1008 policy) is reconnected.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { isTerminalCloseCode } from '../src/index.js'

test('only 4003 and 4004 are terminal close codes', () => {
  for (const code of [4003, 4004]) assert.equal(isTerminalCloseCode(code), true, String(code))
  for (const code of [1000, 1001, 1006, 1008, 1011, 1012, 1013, 4001, 4002, 4005]) {
    assert.equal(isTerminalCloseCode(code), false, String(code))
  }
})
