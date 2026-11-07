"""base_url is the deployment origin: a trailing /api/v1 is dropped."""

from __future__ import annotations

import pytest

import bma_executor_sdk as bx


@pytest.mark.parametrize("base_url", ["https://bma.test", "https://bma.test/", "https://bma.test/api/v1", "https://bma.test/api/v1/"])
def test_base_url_is_the_origin(base_url: str) -> None:
    assert bx.DEFAULT_BASE_URL == "https://api.bma.beeos.ai"
    client = bx.ExecutorClient(
        environment_id="env", environment_key="k", base_url=base_url,
        executor={"name": "t", "os": "linux", "arch": "arm64", "version": "0"},
    )
    assert client._base_url == "https://bma.test"  # pyright: ignore[reportPrivateUsage]


_EXECUTOR = {"name": "t", "os": "linux", "arch": "arm64", "version": "0"}


async def _token_path(monkeypatch: pytest.MonkeyPatch, *, environment_id: str, machine_id: str = "", env: str = "") -> str:
    import httpx

    monkeypatch.setenv(bx.MACHINE_ID_ENV, env)
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.raw_path.decode().split("?")[0])
        return httpx.Response(200, json={"executor_id": "exec_t", "token": "jwt", "expires_in": 300, "ws_url": "ws://x"})

    client = bx.ExecutorClient(
        environment_id=environment_id, machine_id=machine_id, environment_key="k", base_url="https://bma.test",
        executor=_EXECUTOR,  # type: ignore[arg-type]
        http_client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    await client._fetch_token()  # pyright: ignore[reportPrivateUsage]
    assert len(seen) == 1
    return seen[0]


async def test_token_exchange_path_follows_the_resource(monkeypatch: pytest.MonkeyPatch) -> None:
    assert await _token_path(monkeypatch, environment_id="env") == "/v1/environments/env/token"
    assert await _token_path(monkeypatch, environment_id="", machine_id="mach_1") == "/v1/machines/mach_1/token"
    assert await _token_path(monkeypatch, environment_id="", machine_id="a/b") == "/v1/machines/a%2Fb/token"
    # No id: the legacy alias keeps serving a machine daemon token.
    assert await _token_path(monkeypatch, environment_id="") == "/api/v1/executors/token"


async def test_machine_id_comes_from_bma_machine_id(monkeypatch: pytest.MonkeyPatch) -> None:
    assert bx.MACHINE_ID_ENV == "BMA_MACHINE_ID"
    assert await _token_path(monkeypatch, environment_id="", env="mach_env") == "/v1/machines/mach_env/token"
    # An explicit environment_id wins over the ambient variable.
    assert await _token_path(monkeypatch, environment_id="env", env="mach_env") == "/v1/environments/env/token"


def test_environment_id_and_machine_id_are_alternatives() -> None:
    with pytest.raises(ValueError, match="environment_id and machine_id"):
        bx.ExecutorClient(environment_id="env", machine_id="mach_1", environment_key="k", executor=_EXECUTOR)  # type: ignore[arg-type]
