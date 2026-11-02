import contextlib
import fcntl
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs

import siwc_refresh


class RefreshTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "credentials.json"
        self.state = {"client_id": "issued-client", "access_token": "old-access",
                      "refresh_token": "old-refresh", "expires_at": 1000,
                      "gateway_admin_token": "gateway-admin", "ext_agent_host_id": "vm-host"}
        siwc_refresh.save_state(self.path, self.state)
        self.clock = patch.object(siwc_refresh.time, "time", return_value=1000).start()
        self.addCleanup(patch.stopall)
        self.request_json = siwc_refresh.http_json
        self.http = patch.object(siwc_refresh, "http_json").start()
        self.channel = (200, {"success": True, "data": {"id": 17, "type": 1, "channel_info": {}}})
        self.replacement = (200, {"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600})
        self.ok = (200, {"success": True})

    def run_refresh(self):
        with contextlib.redirect_stdout(io.StringIO()):
            siwc_refresh.refresh_once(self.path)

    def read_state(self):
        return json.loads(self.path.read_text())

    def test_rotation_and_correct_client_resource_and_gateway_key(self):
        self.http.side_effect = [self.channel, self.replacement, self.ok]
        self.run_refresh()
        saved = self.read_state()
        self.assertEqual((saved["access_token"], saved["refresh_token"], saved["expires_at"]),
                         ("new-access", "new-refresh", 4600))
        self.assertEqual(saved["client_id"], "issued-client")
        self.assertEqual(saved["ext_agent_host_id"], "vm-host")
        self.assertFalse(saved["gateway_sync_pending"])
        self.assertEqual(os.stat(self.path).st_mode & 0o777, 0o600)
        form = parse_qs(self.http.call_args_list[1].args[2])
        self.assertEqual(form, {"grant_type": ["refresh_token"], "client_id": ["issued-client"],
                                "refresh_token": ["old-refresh"], "resource": ["https://api.openai.com/v1"]})
        self.assertEqual(json.loads(self.http.call_args_list[2].args[2]),
                         {"id": 17, "type": 1, "key": "new-access"})

    def test_gateway_failure_retries_saved_token_without_rotating_again(self):
        self.http.side_effect = [self.channel, self.replacement, (500, {})]
        with self.assertRaisesRegex(RuntimeError, "key update failed"):
            self.run_refresh()
        self.assertEqual(self.read_state()["refresh_token"], "new-refresh")
        self.assertTrue(self.read_state()["gateway_sync_pending"])
        self.http.reset_mock(side_effect=True)
        self.http.side_effect = [self.channel, self.ok]
        self.run_refresh()
        self.assertEqual([call.args[0] for call in self.http.call_args_list], ["GET", "PUT"])
        self.http.reset_mock(side_effect=True)
        self.run_refresh()
        self.http.assert_not_called()

    def test_temporary_and_malformed_errors_keep_existing_credentials(self):
        for response in [(503, {}), (200, {"access_token": "partial"}), (400, {"error": "invalid_client"})]:
            with self.subTest(response=response):
                self.http.side_effect = [self.channel, response]
                with self.assertRaises(RuntimeError):
                    self.run_refresh()
                self.assertEqual(self.read_state(), self.state)

    def test_terminal_refresh_error_requires_sign_in_without_repeated_rotation(self):
        self.http.side_effect = [self.channel, (400, {"error": "refresh_token_reused"})]
        with self.assertRaisesRegex(RuntimeError, "sign in again"):
            self.run_refresh()
        saved = self.read_state()
        self.assertNotIn("refresh_token", saved)
        self.assertNotIn("access_token", saved)
        self.assertEqual(saved["client_id"], "issued-client")
        self.http.reset_mock()
        with self.assertRaisesRegex(RuntimeError, "sign-in is required"):
            self.run_refresh()
        self.http.assert_not_called()

    def test_private_storage_and_single_refresh_owner(self):
        self.path.chmod(0o644)
        with self.assertRaisesRegex(RuntimeError, "0600"):
            self.run_refresh()
        self.path.chmod(0o600)
        with self.path.with_suffix(".lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.run_refresh()
        self.http.assert_not_called()

    def test_do_not_refresh_before_server_allowed_time(self):
        self.state["expires_at"] = 1200
        self.state["earliest_refresh_at"] = 1100
        siwc_refresh.save_state(self.path, self.state)
        self.http.side_effect = [self.channel, self.ok]
        self.run_refresh()
        self.assertEqual([call.args[0] for call in self.http.call_args_list], ["GET", "PUT"])
        self.assertEqual(self.read_state()["refresh_token"], "old-refresh")

    def test_wrong_channel_does_not_rotate_credentials(self):
        self.http.return_value = (200, {"success": True, "data": {"id": 17, "type": 57}})
        with self.assertRaisesRegex(RuntimeError, "single-key OpenAI"):
            self.run_refresh()
        self.assertEqual(self.http.call_count, 1)
        self.assertEqual(self.read_state(), self.state)

    def test_no_plaintext_remote_credential_transport(self):
        with self.assertRaisesRegex(RuntimeError, "HTTPS"):
            self.request_json("GET", "http://example.com/api/channel/17")


if __name__ == "__main__":
    unittest.main()
