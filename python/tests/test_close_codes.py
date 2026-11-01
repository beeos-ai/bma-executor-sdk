"""Only the contract's stop codes end the client: 4003 unsupported protocol
and 4004 replaced. A restarting server (4002 runtime ended, 1001/1012/1013,
4005 drained, 1008 policy) is reconnected."""

from __future__ import annotations

import bma_executor_sdk as bx


def test_only_4003_and_4004_are_terminal() -> None:
    assert bx.TERMINAL_CLOSE_CODES == frozenset({4003, 4004})
