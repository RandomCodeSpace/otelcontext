"""Startup helper regressions; mocked failures never launch a process."""
import importlib.util
import io
from pathlib import Path
import unittest
from unittest.mock import MagicMock, patch
import urllib.error

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("smoke_startup", ROOT / "scripts/smoke-startup.py")
assert SPEC is not None and SPEC.loader is not None
smoke = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(smoke)


class StartupSmokeTests(unittest.TestCase):
    def test_executable_argument_is_rejected_before_launch(self):
        with patch.object(smoke.sys, "argv", ["smoke-startup.py", "/tmp/arbitrary"]), \
                patch.object(smoke.subprocess, "run") as migrate, \
                patch.object(smoke.subprocess, "Popen") as launch:
            with self.assertRaisesRegex(SystemExit, "no arguments"):
                smoke.main()
        migrate.assert_not_called()
        launch.assert_not_called()

    def test_timeout_preserves_transport_error_and_cleans_up(self):
        failure = urllib.error.URLError("startup refused")
        process = MagicMock()
        process.poll.return_value = None
        with patch.object(smoke.sys, "argv", ["smoke-startup.py"]), \
                patch.object(smoke.subprocess, "run") as migrate, \
                patch.object(smoke.subprocess, "Popen", return_value=process) as launch, \
                patch.object(smoke.urllib.request, "urlopen", side_effect=failure), \
                patch.object(smoke.time, "monotonic", side_effect=[0, 31]), \
                patch("sys.stdout", new_callable=io.StringIO):
            with self.assertRaises(TimeoutError) as caught:
                smoke.main()
        self.assertIs(caught.exception.__cause__, failure)
        binary = str(ROOT / ".ci-bin" / "otelcontext")
        self.assertEqual(migrate.call_args.args[0], [binary, "migrate", "up"])
        self.assertEqual(launch.call_args.args[0], [binary])
        process.kill.assert_called_once()
        process.wait.assert_called_once_with(timeout=5)

    def test_transient_startup_failure_recovers(self):
        process = MagicMock()
        process.poll.side_effect = [None, None, None, 0]
        process.wait.return_value = 0
        response = MagicMock()
        response.__enter__.return_value.status = 200
        with patch.object(smoke.sys, "argv", ["smoke-startup.py"]), \
                patch.object(smoke.subprocess, "run"), \
                patch.object(smoke.subprocess, "Popen", return_value=process), \
                patch.object(smoke.urllib.request, "urlopen", side_effect=[
                    urllib.error.URLError("not listening yet"), response, response]), \
                patch.object(smoke.time, "monotonic", side_effect=[0, 1]), \
                patch.object(smoke.time, "sleep") as sleep, \
                patch("sys.stdout", new_callable=io.StringIO) as output:
            smoke.main()
        self.assertIn("PASS:", output.getvalue())
        sleep.assert_called_once_with(0.2)
        process.terminate.assert_called_once()
        process.kill.assert_not_called()
        process.wait.assert_called_once_with(timeout=30)


if __name__ == "__main__":
    unittest.main()
