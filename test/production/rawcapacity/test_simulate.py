import argparse
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

from test.production.rawcapacity import simulate as sim


class SimulationTests(unittest.TestCase):
    def test_timestamp_precision_and_ids(self):
        self.assertEqual(sim.shifted("2026-10-18 19:00:00.123456789+00:00", 86400),
                         "2026-10-19 19:00:00.123456789+00:00")
        self.assertIsNone(sim.shifted(None, 3600))
        self.assertEqual(sim.identity("", 2), "")
        self.assertEqual(sim.identity("abc", 2), "b36cb309eb00982a96a4f9aca2c81849")
        self.assertNotEqual(sim.identity("abc", 2), sim.identity("abc", 3))

    @unittest.skipUnless(os.environ.get("RAWCAPACITY_HELPER"), "build helper and set RAWCAPACITY_HELPER")
    def test_indexed_replication_and_rolling_production_purge(self):
        helper = Path(os.environ["RAWCAPACITY_HELPER"]).resolve()
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "source"
            sim.helper_result(helper, "init", source)
            db = source / "raw.db"
            conn = sqlite3.connect(db)
            for hour in (0, 1):
                ts = f"2026-10-18 {19 + hour}:01:02.123456789+00:00"
                tid = f"{hour + 1:032x}"
                with conn:
                    conn.execute("INSERT INTO traces(tenant_id,trace_id,service_name,timestamp,created_at,updated_at) VALUES('default',?,'svc',?,?,?)", (tid, ts, ts, ts))
                    conn.execute("INSERT INTO spans(tenant_id,trace_id,span_id,service_name,start_time,end_time,attributes_json) VALUES('default',?,'span','svc',?,?,?)", (tid, ts, ts, b'{"preserved":"span"}'))
                    conn.execute("INSERT INTO logs(tenant_id,trace_id,span_id,service_name,timestamp,body,attributes_json) VALUES('default',?,'span','svc',?,'retained token',?)", (tid, ts, b'{"preserved":"log"}'))
                    conn.execute("INSERT INTO traces(tenant_id,trace_id,service_name,timestamp,truncated,retained_span_count,observed_span_count) VALUES('default',?,'svc',?,1,0,3)", (f"{hour + 100:032x}", ts))
            with conn:
                # Crop complete trace groups by the parent's timestamp. This
                # child crosses into the source window but its parent does not.
                conn.execute("INSERT INTO traces(tenant_id,trace_id,service_name,timestamp) VALUES('default','outside','svc','2026-10-18 18:59:59+00:00')")
                conn.execute("INSERT INTO spans(tenant_id,trace_id,span_id,start_time) VALUES('default','outside','span','2026-10-18 19:00:00+00:00')")
            conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            conn.close()
            args = argparse.Namespace(source=db, helper=helper, out=root / "output", source_start="2026-10-18T19:00:00Z",
                                      source_hours=2, end="2026-10-18T21:00:00Z", hours=4, roll_hours=2, max_bytes=64 << 20)
            result = sim.simulate(args)
            self.assertTrue(result["passed"])
            self.assertEqual(result["seed"]["counts"], {"traces": 9, "spans": 5, "logs": 5})
            self.assertEqual(result["final"]["counts"], {"traces": 8, "spans": 4, "logs": 4})
            self.assertTrue(result["fts_sentinel_insert_delete"])
            self.assertTrue(result["fts_integrity"])
            self.assertTrue(result["source_unchanged"])
            self.assertEqual(result["template_zero_span_headers"], 2)
            with self.assertRaises(ValueError):
                sim.simulate(args)  # Never overwrite even our own evidence.


if __name__ == "__main__":
    unittest.main()
