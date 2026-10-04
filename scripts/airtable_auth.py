#!/usr/bin/env python3
"""
Shared Airtable auth for koei-auctions pipeline scripts.

Credential resolution order (no secrets are ever hardcoded or committed):
  1. AIRTABLE_API_KEY environment variable (use a .env file locally — gitignored)
  2. Hatch runtime dynamic credential (only present on the agent host)

Exposes:
  api_headers() -> dict with Authorization + Content-Type
  api(method, path, body=None) -> parsed JSON response
"""
import json
import os
import sys
import urllib.request

BASE_URL = "https://api.airtable.com/v0"


def _resolve_key():
    key = os.environ.get("AIRTABLE_API_KEY")
    if key:
        return key, False
    # Hatch runtime flow (agent host only)
    try:
        sys.path.insert(0, "/opt/hatch/skills/skill-creator/bin")
        from dynamic_credentials import dynamic_credential_entry
        return dynamic_credential_entry("custom.airtable")["surrogate"], True
    except Exception as e:
        raise RuntimeError(
            "No Airtable credential: set AIRTABLE_API_KEY or run on the agent host"
        ) from e


_API_KEY, _IS_SURROGATE = _resolve_key()


def _maybe_add_surrogate(req):
    if _IS_SURROGATE:
        sys.path.insert(0, "/opt/hatch/skills/skill-creator/bin")
        from dynamic_credentials import add_surrogate_to_request
        add_surrogate_to_request(req, "custom.airtable",
                                 allowed_hosts=["api.airtable.com"])


def api_headers():
    # Never log or print the key.
    return {"Authorization": f"Bearer {_API_KEY}", "Content-Type": "application/json"}


def api(method, path, body=None):
    url = f"{BASE_URL}/{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    if _IS_SURROGATE:
        _maybe_add_surrogate(req)
    else:
        req.add_header("Authorization", f"Bearer {_API_KEY}")
    with urllib.request.urlopen(req, timeout=90) as r:
        return json.load(r)
