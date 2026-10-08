"""Accept only the two known failures in the experimental #14548 backport."""
from pathlib import Path
import re
import sys

root = Path(sys.argv[1])
log = (root / "regression.log").read_text()
expected = [
    ("PASS", "RAControl"),
    ("PASS", "IPv4MetricControl"),
    ("PASS", "IPv4LinkMove"),
    ("FAIL", "BGPBesideRA"),
    ("PASS", "NextHopReplacement"),
    ("PASS", "ForeignProtocolOwnership/repro0"),
    ("PASS", "ForeignProtocolOwnership/missing-link"),
    ("FAIL", "IdempotentDelete"),
]
assert (root / "exit-code").read_text().strip() == "1", "expected test failure exit"
actual = re.findall(r"--- (PASS|FAIL|SKIP): TestUpstreamRouteRegression/([^ ]+) ", log)
assert actual == expected, actual
assert log.count("REPRO_RA_BGP_COLLISION:") == 1, "missing EEXIST evidence"
assert log.count("REPRO_STALE_DELETE:") == 1, "missing ESRCH evidence"
assert "Error Trace:" not in log and "panic:" not in log, "unexpected failure"
suite = (root / "upstream-tests.log").read_text()
assert "--- FAIL:" not in suite and "--- SKIP:" not in suite
for name in ("TestRouteSpecSuite", "TestRouteMergeSuite"):
    assert re.search(r"^--- PASS: " + name + r" ", suite, re.M), name
assert suite.rstrip().endswith("PASS"), "suite did not finish"
print("Upstream #14548 backport: 6 behavior cases pass; RA EEXIST and stale-delete ESRCH remain. Route/merge suites pass. Not a promotion result.")
