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


@pytest.mark.parametrize(("environment_id", "path"), [("", "/api/v1/executors/token"), ("env", "/v1/executors/token")])
async def test_token_exchange_path_follows_the_credential(environment_id: str, path: str) -> None:
    import httpx

    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.path)
        return httpx.Response(200, json={"executor_id": "exec_t", "token": "jwt", "expires_in": 300, "ws_url": "ws://x"})

    client = bx.ExecutorClient(
        environment_id=environment_id, environment_key="k", base_url="https://bma.test",
        executor={"name": "t", "os": "linux", "arch": "arm64", "version": "0"},
        http_client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    await client._fetch_token()  # pyright: ignore[reportPrivateUsage]
    assert seen == [path]
