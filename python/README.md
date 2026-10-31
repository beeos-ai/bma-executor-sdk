# bma-executor-sdk (Python)

asyncio executor (`ExecutorClient`), harness (`Harness`) and agent ends of the
BMA executor protocol `bma.executor.v1.1`, with TypedDict protocol types
generated from the BMA contract (`contracts/bma/executor`). Transports:
WebSocket, WebSocket over a Unix domain socket (`unix://`), in-process.

```sh
pip install bma-executor-sdk
```

```python
import bma_executor_sdk as bx
```

See the [repository README](../../README.md#python-executor) and
[examples](examples). The REST/façade client is the separate `bma-sdk` package.
