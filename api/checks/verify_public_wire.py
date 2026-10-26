"""Offline Relay v1 fixture checks; no server, signer, key custody or networking."""
import base64
from datetime import datetime
import hashlib
import json
from pathlib import Path
import sys

from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource
from referencing.exceptions import NoSuchResource

ROOT = Path(__file__).resolve().parents[1]


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate fixture key")
        result[key] = value
    return result


def load(path):
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique)


def closed(uri):
    raise NoSuchResource(ref=uri)


def invalid_number(value):
    # Every numeric field in these Go wire structs is an integer. JSON Schema
    # sees 1.0 == 1 after parsing, while encoding/json rejects 1.0 for a Go int.
    raise ValueError("non-integer or non-JSON number in wire input")


SCHEMA = load(ROOT / "schemas/public-wire.schema.json")
Draft202012Validator.check_schema(SCHEMA)
REGISTRY = Registry(retrieve=closed).with_resource(
    SCHEMA["$id"], Resource.from_contents(SCHEMA)
)
FORMATS = FormatChecker()


@FORMATS.checks("date-time", raises=ValueError)
def valid_timestamp(value):
    # jsonschema's optional RFC3339 dependency need not be installed. The schema
    # constrains the Go RFC3339Nano syntax; datetime checks real calendar dates.
    if isinstance(value, str):
        datetime.fromisoformat(value.replace("Z", "+00:00"))
    return True


def validate_shape(definition, value):
    if definition not in SCHEMA["$defs"]:
        raise ValueError("unknown fixture definition")
    selected = {
        "$schema": SCHEMA["$schema"],
        "$ref": SCHEMA["$id"] + "#/$defs/" + definition,
    }
    Draft202012Validator(
        selected, registry=REGISTRY, format_checker=FORMATS
    ).validate(value)


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def raw_url(value, expected_length):
    decoded = base64.b64decode(value + "=" * (-len(value) % 4), altchars=b"-_", validate=True)
    if len(decoded) != expected_length:
        raise ValueError("invalid public material size")
    return decoded


def validate_semantics(definition, value, at=None):
    validate_shape(definition, value)
    if definition == "SigningTrustBundle":
        seen = set()
        for key in value["keys"]:
            if key["key_id"] in seen:
                raise ValueError("duplicate trust key")
            seen.add(key["key_id"])
            digest = hashlib.sha256(raw_url(key["public_key"], 32)).digest()
            derived = "ed25519:" + base64.urlsafe_b64encode(digest).decode().rstrip("=")
            if key["key_id"] != derived:
                raise ValueError("trust key identity mismatch")
            if key.get("not_before") and key.get("not_after"):
                if timestamp(key["not_after"]) <= timestamp(key["not_before"]):
                    raise ValueError("invalid trust validity window")
        if value["active_key_id"] not in seen:
            raise ValueError("missing active trust key")
    elif definition == "Credential":
        # A synthetic signature's shape is checked, never treated as verified.
        raw_url(value["signature"], 64)
        if at is None or timestamp(at) >= timestamp(value["expires_at"]):
            raise ValueError("credential expiry reached or fixture clock missing")
    elif definition == "EndpointSnapshot":
        ids = [endpoint["id"] for endpoint in value["endpoints"] or []]
        if len(ids) != len(set(ids)):
            raise ValueError("duplicate endpoint identity")
    else:
        raise ValueError("unknown semantic fixture kind")


def validate_line(vector):
    raw = bytes.fromhex(vector["wire_hex"])
    if not raw.endswith(b"\n") or raw.count(b"\n") != 1 or len(raw) > 147456:
        raise ValueError("invalid line framing or bound")
    value = json.loads(
        raw[:-1].decode("utf-8"), object_pairs_hook=unique,
        parse_float=invalid_number, parse_constant=invalid_number,
    )
    validate_shape(vector["definition"], value)
    if "opaque_payload_hex" in vector or "opaque_payload_sha256" in vector:
        payload = base64.b64decode(value["payload"], validate=True)
        if not 1 <= len(payload) <= 65536:
            raise ValueError("opaque payload bound changed")
        if "opaque_payload_hex" in vector and payload.hex() != vector["opaque_payload_hex"]:
            raise ValueError("opaque payload or bound changed")
        if "opaque_payload_sha256" in vector:
            if hashlib.sha256(payload).hexdigest() != vector["opaque_payload_sha256"]:
                raise ValueError("opaque payload digest changed")


def check(name, valid, action):
    try:
        action()
    except (ValueError, UnicodeDecodeError, json.JSONDecodeError) as error:
        if valid:
            raise RuntimeError("positive fixture failed: " + name) from error
    except Exception as error:
        # JSON Schema validation is a negative outcome; dependency/programming
        # errors must fail the suite rather than masquerade as fixture rejection.
        from jsonschema.exceptions import ValidationError
        if not isinstance(error, ValidationError) or valid:
            raise RuntimeError("fixture failure: " + name) from error
    else:
        if not valid:
            raise RuntimeError("negative fixture accepted: " + name)


def main():
    cases = load(ROOT / "fixtures/cases.json")
    vectors = load(ROOT / "wire-vectors.json")
    for case in cases:
        definition = case["schema"].split("#/$defs/", 1)[1]
        check(case["name"], case["valid"], lambda c=case, d=definition: validate_shape(d, load(ROOT / c["instance"])))
    for vector in vectors["byte_vectors"]:
        check(vector["name"], vector["valid"], lambda v=vector: validate_line(v))
    for vector in vectors["semantic_vectors"]:
        check(vector["name"], vector["valid"], lambda v=vector: validate_semantics(v["definition"], v["value"], v.get("at")))
    print(json.dumps({
        "schema_fixtures": len(cases),
        "byte_vectors": len(vectors["byte_vectors"]),
        "semantic_vectors": len(vectors["semantic_vectors"]),
        "python": sys.version.split()[0],
        "result": "PASS",
        "signature_verification": "not-verified",
    }))


if __name__ == "__main__":
    main()
