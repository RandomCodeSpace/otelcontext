"""Release source preflight contract. All GitHub responses are local fixtures."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SHA = "a" * 40


class ReleaseSourceTests(unittest.TestCase):
    def setUp(self):
        self.branch = {"protected": True, "protection": {
            "required_status_checks": {"contexts": ["build · vet · test", "SonarCloud Code Analysis"]}}}
        self.workflow_run = {"id": 15, "run_attempt": 2, "head_sha": SHA, "head_branch": "main",
                    "event": "push", "path": ".github/workflows/ci.yml",
                    "status": "completed", "conclusion": "success"}
        self.job = {"name": "main validation", "status": "completed",
                    "conclusion": "success", "html_url": "https://example.test/job/15"}

    def invoke(self, runs=None, jobs=None, branch=None, sha=SHA):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory)
            (fixture / "branch.json").write_text(json.dumps(self.branch if branch is None else branch))
            (fixture / "runs.json").write_text(json.dumps({
                "workflow_runs": [self.workflow_run] if runs is None else runs}))
            (fixture / "jobs.jsonl").write_text("\n".join(
                json.dumps(job) for job in ([self.job] if jobs is None else jobs)))
            gh = fixture / "gh"
            gh.write_text("""#!/usr/bin/env python3
import os, pathlib, sys
args = sys.argv[1:]
root = pathlib.Path(os.environ["CI_SOURCE_FIXTURE"])
with (root / "calls").open("a") as stream:
    stream.write(" ".join(args) + "\\n")
if any(a.endswith("/branches/main") for a in args): name = "branch.json"
elif any(a.endswith("/workflows/ci.yml/runs") for a in args): name = "runs.json"
elif any("/attempts/" in a and "/jobs?" in a for a in args): name = "jobs.jsonl"
else: raise SystemExit("unexpected gh command: " + repr(args))
print((root / name).read_text())
""")
            gh.chmod(0o755)
            result = subprocess.run(
                ["bash", str(ROOT / "scripts/check-release-source.sh"), sha],
                env=dict(os.environ, PATH=f"{fixture}:{os.environ['PATH']}",
                         GITHUB_REPOSITORY="example/otelcontext", CI_SOURCE_FIXTURE=directory),
                text=True, capture_output=True, timeout=10)
            calls = (fixture / "calls").read_text() if (fixture / "calls").exists() else ""
            return result, calls

    def test_exact_main_ignores_pr_only_contexts(self):
        result, calls = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)[0]["sha"], SHA)
        self.assertIn("/runs/15/attempts/2/jobs", calls)

    def test_latest_run_must_succeed(self):
        for status, conclusion in [("in_progress", None), ("queued", None),
                                   ("completed", "failure"), ("completed", "cancelled"),
                                   ("completed", "skipped")]:
            with self.subTest(status=status, conclusion=conclusion):
                latest = dict(self.workflow_run, id=16, status=status, conclusion=conclusion)
                result, _ = self.invoke(runs=[latest, self.workflow_run])
                self.assertNotEqual(result.returncode, 0)

    def test_wrong_source_event_branch_or_workflow(self):
        for change in [{"head_sha": "b" * 40}, {"event": "pull_request"},
                       {"event": "workflow_dispatch"}, {"head_branch": "feature"},
                       {"path": ".github/workflows/other.yml"}]:
            with self.subTest(change=change):
                result, _ = self.invoke(runs=[dict(self.workflow_run, **change)])
                self.assertNotEqual(result.returncode, 0)

    def test_missing_or_unsuccessful_main_job(self):
        cases = [[], [self.job, self.job], [dict(self.job, name="build · vet · test")]]
        cases += [[dict(self.job, conclusion=value)]
                  for value in [None, "skipped", "failure", "cancelled"]]
        cases += [[dict(self.job, status="in_progress")]]
        for jobs in cases:
            with self.subTest(jobs=jobs):
                result, _ = self.invoke(jobs=jobs)
                self.assertNotEqual(result.returncode, 0)

    def test_protected_main_and_required_checks(self):
        for branch in [{}, dict(self.branch, protected=False),
                       {"protected": True, "protection": {"required_status_checks": {"contexts": []}}}]:
            with self.subTest(branch=branch):
                result, _ = self.invoke(branch=branch)
                self.assertNotEqual(result.returncode, 0)

    def test_bad_sha_rejected_before_network(self):
        result, calls = self.invoke(sha="not-a-sha")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, "")


if __name__ == "__main__":
    unittest.main()
