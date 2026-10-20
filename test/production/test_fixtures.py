import argparse
import io
import json
import pathlib
import tempfile
import unittest.mock

import fixtures


class FixtureTests(unittest.TestCase):
    def test_history_counts_names_and_reset(self):
        specs = fixtures.series_specs(200, 20, "loadsim-svc-")
        self.assertEqual(len(specs), 706)
        self.assertEqual(len(specs) * 7 * 86400 // 60, 7116480)
        self.assertEqual(len(specs) * 8 * 86400 // 60, 8133120)
        values = {name: value for name, value in specs}
        counter = next(v for k, v in values.items() if k.startswith('{"http.server.request.count",') and '"service.name"="loadsim-svc-000"' in k)
        reset = next(v for k, v in values.items() if k.startswith('{"custom.reset.counter",') and '"service.name"="loadsim-svc-000"' in k)
        self.assertEqual(counter(61) - counter(60), 60)
        self.assertEqual([reset(i) for i in (58, 59, 60, 61, 62)], [3540, 3600, 60, 120, 180])
        self.assertEqual(len(set(values)), len(specs))
        checks = fixtures.expected_queries(0, 8 * 86400, 200, 20, "loadsim-svc-")
        bounds = next(c for c in checks if c["name"] == "raw_history_bounds")
        self.assertEqual((bounds["first"], bounds["last"], bounds["samples"]), (0, 8 * 86400 - 60, 11520))

    def test_block_timestamps_are_seconds_and_exclusive(self):
        with tempfile.TemporaryDirectory() as temp:
            path = pathlib.Path(temp) / "block.om"
            specs = fixtures.series_specs(2, 1, "loadsim-svc-")
            count = fixtures.write_block(path, specs, 7200, 14400, 0)
            lines = path.read_text().splitlines()
        self.assertEqual(count, 17 * 120)
        self.assertEqual(lines[-1], "# EOF")
        self.assertEqual(int(lines[0].rsplit(" ", 1)[1]), 7200)
        self.assertEqual(int(lines[-2].rsplit(" ", 1)[1]), 14340)
        self.assertEqual(len(lines), count + 1)

    def test_refuse_nonempty_and_symlink_targets(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = pathlib.Path(temp) / "fixture"
            with fixtures.empty_fixture(directory):
                (directory / "user-data").write_text("keep")
            with self.assertRaises(ValueError):
                with fixtures.empty_fixture(directory):
                    self.fail("entered existing output")
            link = pathlib.Path(temp) / "link"
            link.symlink_to(directory, target_is_directory=True)
            with self.assertRaises(ValueError):
                with fixtures.empty_fixture(link):
                    self.fail("entered symlink output")
            self.assertEqual((directory / "user-data").read_text(), "keep")

    def test_host_otlp_identity_and_values(self):
        payload = fixtures.host_payload(20, 123456789)
        self.assertEqual(len(payload["resourceMetrics"]), 20)
        for i, resource in enumerate(payload["resourceMetrics"]):
            attrs = {a["key"]: a["value"] for a in resource["resource"]["attributes"]}
            self.assertNotIn("service.name", attrs)
            self.assertEqual(attrs["host.id"], {"stringValue": f"mock-host-id-{i:03d}"})
            self.assertEqual(attrs["service.instance.id"], attrs["host.id"])
            self.assertEqual(attrs["host.cpu.logical.count"], {"intValue": "8"})
            self.assertEqual(attrs["host.memory.total"], {"intValue": str(32 << 30)})
            metrics = resource["scopeMetrics"][0]["metrics"]
            self.assertEqual(tuple(m["name"] for m in metrics), fixtures.HOST_METRICS)
            self.assertTrue(all(m["gauge"]["dataPoints"][0]["timeUnixNano"] == "123456789" for m in metrics))

    def test_otlp_rejections_fail(self):
        for status, body in [(204, b""), (200, b'{"partialSuccess":{"rejectedDataPoints":"2"}}'),
                             (200, b'{"partialSuccess":{"errorMessage":"lost data"}}'),
                             (200, b'{"partial_success":{"rejected_data_points":1}}'), (200, b'[]')]:
            with self.subTest(status=status, body=body):
                with self.assertRaises(ValueError):
                    fixtures.validate_otlp_response(status, body)
        fixtures.validate_otlp_response(200, b'{}')
        fixtures.validate_otlp_response(200, b'{"partialSuccess":{"rejectedDataPoints":"0"}}')
        with self.assertRaises(ValueError):
            fixtures.http_client("http://example.test", None)

    def test_host_failure_report_does_not_count_partial_ack(self):
        response = unittest.mock.MagicMock()
        response.__enter__.return_value = response
        response.status = 200
        response.read.return_value = b'{"partialSuccess":{"rejectedDataPoints":"1"}}'
        client = unittest.mock.Mock()
        client.open.return_value = response
        with tempfile.TemporaryDirectory() as temp:
            report = pathlib.Path(temp) / "report.ndjson"
            args = argparse.Namespace(hosts=20, interval=10, duration=1, url="https://example.test", ca_file=None, timeout=1, report=str(report))
            with unittest.mock.patch.object(fixtures, "http_client", return_value=client), unittest.mock.patch("sys.stdout", new_callable=io.StringIO):
                with self.assertRaises(ValueError):
                    fixtures.send_hosts(args)
            summary = json.loads(report.read_text())["summary"]
            self.assertEqual(summary["requests_attempted"], 1)
            self.assertEqual(summary["points_attempted"], 100)
            self.assertEqual(summary["requests_accepted"], 0)
            self.assertEqual(summary["points_accepted"], 0)
            self.assertIn("error", summary)


if __name__ == "__main__":
    unittest.main()
