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
