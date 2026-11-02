#!/usr/bin/env python3
"""Run once per minute for channel 17; the private JSON file contains the SIWC
token response, issued client_id, Unix expires_at, and gateway_admin_token.
gateway_base_url defaults to http://127.0.0.1:8000. Stop other session refreshers
before importing the file into a server-owned directory with mode 0700/0600.
"""

import fcntl
import http.client
import json
import math
import os
from pathlib import Path
import stat
import sys
import tempfile
import time
from urllib.parse import urlencode, urlsplit


TOKEN_URL = "https://auth.openai.com/api/accounts/oauth/token"
TERMINAL_ERRORS = {
    "invalid_grant", "invalid_refresh_token", "token_expired",
    "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused",
}


def http_json(method, url, body=None, headers=None):
    target = urlsplit(url)
    if target.username or target.password or target.fragment:
        raise RuntimeError("Invalid request URL")
    if target.scheme == "https":
        connection = http.client.HTTPSConnection(target.hostname, target.port, timeout=20)
    elif target.scheme == "http" and target.hostname in {"127.0.0.1", "::1"}:
        connection = http.client.HTTPConnection(target.hostname, target.port, timeout=20)
    else:
        raise RuntimeError("HTTPS is required outside loopback")
    try:
        path = target.path or "/"
        if target.query:
            path += "?" + target.query
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        # Do not follow redirects carrying credentials or log response bodies.
        raw = response.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise RuntimeError("Response exceeds size limit")
        data = json.loads(raw)
        if not isinstance(data, dict):
            raise RuntimeError("Invalid JSON response")
        return response.status, data
    finally:
        connection.close()


def save_state(path, state):
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix=".siwc-")
    try:
        with os.fdopen(fd, "w") as output:
            json.dump(state, output)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def refresh_once(path):
    path = Path(path)
    parent = path.parent.stat()
    if parent.st_uid != os.geteuid() or parent.st_mode & 0o077:
        raise RuntimeError("Credential directory must be owned by this user with mode 0700")
    lock_fd = os.open(path.with_suffix(".lock"), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "r+") as lock:
        # shortcut: this lock covers one host; stop laptop/other-host refreshers before import.
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd) as source:
            metadata = os.fstat(source.fileno())
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_uid != os.geteuid() or metadata.st_mode & 0o077:
                raise RuntimeError("Credential file must be owned by this user with mode 0600")
            state = json.load(source)
        if not isinstance(state, dict):
            raise RuntimeError("Invalid credential file")
        if state.get("reauth_required"):
            raise RuntimeError("SIWC sign-in is required; retain the issued client_id")
        for field in ("client_id", "access_token", "refresh_token", "gateway_admin_token"):
            if not isinstance(state.get(field), str) or not state[field].strip():
                raise RuntimeError("Missing required credential fields")
        if state["client_id"] in {"dynamic_agent_client", "app_EMoamEEZ73f0CkXaXp7hrann"}:
            raise RuntimeError("Use the client_id issued for this SIWC session")
        expires_at = state.get("expires_at")
        if type(expires_at) not in (int, float) or not math.isfinite(expires_at):
            raise RuntimeError("expires_at must be a Unix timestamp")
        now = time.time()
        due = expires_at <= now + 300
        if not due and not state.get("gateway_sync_pending", True):
            return

        gateway = state.get("gateway_base_url", "http://127.0.0.1:8000").rstrip("/")
        headers = {"Authorization": "Bearer " + state["gateway_admin_token"], "Content-Type": "application/json"}
        status, channel = http_json("GET", gateway + "/api/channel/17", headers=headers)
        if status != 200 or channel.get("success") is not True:
            raise RuntimeError("Cannot read New API channel 17")
        channel = channel.get("data", {})
        if channel.get("id") != 17 or channel.get("type") != 1 or channel.get("channel_info", {}).get("is_multi_key"):
            raise RuntimeError("Channel 17 must be a single-key OpenAI channel")

        if due:
            earliest = state.get("earliest_refresh_at", 0)
            if type(earliest) not in (int, float) or not math.isfinite(earliest):
                raise RuntimeError("earliest_refresh_at must be a Unix timestamp")
            if now < earliest:
                if not state.get("gateway_sync_pending", True):
                    return
                due = False
        if due:
            form = urlencode({"grant_type": "refresh_token", "client_id": state["client_id"],
                              "refresh_token": state["refresh_token"], "resource": "https://api.openai.com/v1"})
            status, replacement = http_json("POST", TOKEN_URL, form, {"Content-Type": "application/x-www-form-urlencoded"})
            if status != 200:
                error = replacement.get("error")
                if isinstance(error, dict):
                    error = error.get("code")
                if isinstance(error, str) and error in TERMINAL_ERRORS:
                    for field in ("access_token", "refresh_token", "id_token"):
                        state.pop(field, None)
                    state["reauth_required"] = True
                    save_state(path, state)
                    raise RuntimeError("SIWC refresh session ended; sign in again with the saved client_id")
                raise RuntimeError("SIWC refresh failed (HTTP %d); credentials retained" % status)
            if (not all(isinstance(replacement.get(key), str) and replacement[key].strip()
                        for key in ("access_token", "refresh_token"))
                    or type(replacement.get("expires_in")) is not int or replacement["expires_in"] <= 0):
                raise RuntimeError("SIWC token response is missing required fields")
            for field in ("access_token", "refresh_token", "id_token", "scope", "token_type"):
                if field in replacement:
                    state[field] = replacement[field]
            state["expires_at"] = time.time() + replacement["expires_in"]
            state["earliest_refresh_at"] = replacement.get("earliest_refresh_at", 0)
            state["gateway_sync_pending"] = True
            # Persist the rotated refresh token before attempting the gateway update.
            save_state(path, state)

        status, result = http_json("PUT", gateway + "/api/channel/",
                                   json.dumps({"id": 17, "type": 1, "key": state["access_token"]}), headers)
        if status != 200 or result.get("success") is not True:
            raise RuntimeError("New API key update failed; next run will retry the saved token")
        state["gateway_sync_pending"] = False
        save_state(path, state)
        print("Channel 17 credential synchronized")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            raise RuntimeError("Usage: siwc_refresh.py /private/path/credentials.json")
        refresh_once(sys.argv[1])
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
    except Exception:
        # Transport and parser exceptions can contain secrets supplied by a peer.
        print("SIWC refresh failed; inspect file permissions, connectivity, and configuration", file=sys.stderr)
        sys.exit(1)
