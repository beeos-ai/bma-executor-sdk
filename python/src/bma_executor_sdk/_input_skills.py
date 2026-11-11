"""Shape gate. An opted-in handler must verify and install the exact bytes."""
import re
from ._rpc import RPCError
from .generated import JsonRpcErrorCode


def validate_input_skill_request(config: object) -> bool:
    def invalid() -> RPCError:
        return RPCError(JsonRpcErrorCode.InvalidParams, "invalid input_skills")
    if config is None:
        return False
    if not isinstance(config, dict):
        raise invalid()
    if "input_skill_receipt" in config:
        raise RPCError(JsonRpcErrorCode.InvalidParams, "input_skill_receipt is effective-only")
    if "input_skills" not in config:
        return False
    documents = config["input_skills"]
    if not isinstance(documents, list) or not 1 <= len(documents) <= 10:
        raise invalid()
    seen = set()
    total = 0
    keys = {"skill_id", "revision_id", "name", "body", "body_sha256"}
    for doc in documents:
        if not isinstance(doc, dict) or set(doc) != keys or any(not isinstance(v, str) for v in doc.values()):
            raise invalid()
        try:
            size = len(doc["body"].encode("utf-8"))
            name_size = len(doc["name"].encode("utf-8"))
        except UnicodeError as error:
            raise invalid() from error
        total += size
        if (not re.fullmatch(r"skl_[a-zA-Z0-9_-]{1,124}", doc["skill_id"])
            or not re.fullmatch(r"skr_[a-zA-Z0-9_-]{1,124}", doc["revision_id"])
            or not re.fullmatch(r"[0-9a-f]{64}", doc["body_sha256"])
            or not doc["name"].strip() or name_size > 255
            or not doc["body"].strip() or size > 65536 or total > 262144
            or doc["skill_id"] in seen):
            raise invalid()
        seen.add(doc["skill_id"])
    return True
