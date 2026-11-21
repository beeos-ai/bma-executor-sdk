"""Session context reads over the machine token (Go: ``Client.SessionContext``)."""

from __future__ import annotations

import re
from collections import OrderedDict
from typing import Any, TypedDict
from urllib.parse import quote

import httpx

from ._rpc import DaemonHTTPError, MachineTokenRequiredError, SessionContextNotFoundError


class SessionContext(TypedDict):
    """What BMA tells this machine about an agent session it serves
    (GET /api/v1/daemon/processes/{process_id}/session and
    GET /api/v1/daemon/sessions/{session_id}): the session's environment and
    the metadata its creator set, never agent configuration or credentials."""

    id: str
    environment_id: str
    metadata: dict[str, str]
    created_at: str
    """RFC 3339 date-time."""


_MAX_CACHED = 1024
_UNSAFE = re.compile(r"[/\\\x00]")


def _segment(kind: str, value: str) -> str:
    if not value or len(value) > 256 or value in (".", "..") or _UNSAFE.search(value):
        raise ValueError(f"bma_executor_sdk: invalid {kind} {value!r}")
    return quote(value, safe="")


def _clone(context: SessionContext) -> SessionContext:
    return {**context, "metadata": dict(context.get("metadata") or {})}


class SessionContexts:
    """Cached per session (metadata is read once); failures are not cached."""

    def __init__(self, *, base_url: str, environment_id: str, environment_key: str,
                 http_client: httpx.AsyncClient | None, timeout: float) -> None:
        self._base_url = base_url
        self._environment_id = environment_id
        self._environment_key = environment_key
        self._http_client = http_client
        self._timeout = timeout
        self._sessions: OrderedDict[str, SessionContext] = OrderedDict()
        self._processes: OrderedDict[str, str] = OrderedDict()

    async def by_process(self, process_id: str) -> SessionContext:
        known = self._processes.get(process_id)
        if known is not None:
            return await self.by_id(known)
        context = await self._fetch(f"/processes/{_segment('process id', process_id)}/session")
        _put(self._processes, process_id, context["id"])
        return self._remember(context)

    async def by_id(self, session_id: str) -> SessionContext:
        cached = self._sessions.get(session_id)
        if cached is not None:
            return _clone(cached)
        return self._remember(await self._fetch(f"/sessions/{_segment('session id', session_id)}"))

    def _remember(self, context: SessionContext) -> SessionContext:
        cached = self._sessions.get(context["id"])
        if cached is not None:
            return _clone(cached)
        _put(self._sessions, context["id"], context)
        return _clone(context)

    async def _fetch(self, path: str) -> SessionContext:
        if self._environment_id or not self._environment_key.strip():
            raise MachineTokenRequiredError()
        client = self._http_client or httpx.AsyncClient()
        try:
            # follow_redirects stays off: a redirect would forward the machine token.
            response = await client.get(
                f"{self._base_url}/api/v1/daemon{path}",
                headers={"Authorization": f"Bearer {self._environment_key}", "Accept": "application/json"},
                timeout=self._timeout, follow_redirects=False,
            )
        finally:
            if self._http_client is None:
                await client.aclose()
        if response.status_code == 404:
            raise SessionContextNotFoundError()
        if not 200 <= response.status_code < 300:
            raw = response.text[: 64 * 1024]
            try:
                body = response.json()
            except ValueError:
                body = None
            if isinstance(body, dict) and body.get("message"):
                raise DaemonHTTPError(response.status_code, str(body["message"]), str(body.get("code") or ""))
            raise DaemonHTTPError(response.status_code, raw.strip())
        context = response.json()
        if not isinstance(context, dict) or not context.get("id"):
            raise ValueError("bma_executor_sdk: session context response has no id")
        context.setdefault("metadata", {})
        return context  # type: ignore[return-value]


def _put(cache: OrderedDict[str, Any], key: str, value: Any) -> None:
    if key not in cache and len(cache) >= _MAX_CACHED:
        cache.popitem(last=False)
    cache[key] = value
