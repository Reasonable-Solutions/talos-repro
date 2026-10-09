"""Reject infrastructure failures and unrelated assertion failures as evidence."""
from pathlib import Path
import re
import sys

mode, directory = sys.argv[1:]
root = Path(directory)
log = (root / "regression.log").read_text()
status = int((root / "exit-code").read_text())
cases = ["RAControl", "IPv4MetricControl", "IPv4LinkMove", "BGPBesideRA", "NextHopReplacement", "MergedNextHopChange", "InterfaceOwnership",
         "MissingInterfaceOwnership", "IdempotentDelete"]
observed = re.findall(r"^    --- (PASS|FAIL|SKIP): TestRouteRegression/(\w+) ", log, re.M)
expected = [("PASS" if mode == "fix" or name in {"RAControl", "IPv4MetricControl", "IPv4LinkMove"} else "FAIL", name) for name in cases]
assert observed == expected, f"Unexpected subtest results: {observed}\n{log}"
assert status == (0 if mode == "fix" else 1), f"Unexpected exit status {status}\n{log}"
assert "Error Trace:" not in log and "panic:" not in log, log
markers = {"REPRO_RA_BGP_COLLISION:": 1, "REPRO_ADD_BEFORE_WITHDRAWAL:": 1,
           "REPRO_MERGED_NEXT_HOP:": 1, "REPRO_WRONG_INTERFACE_DELETED:": 2, "REPRO_STALE_DELETE:": 1}
for marker, count in markers.items():
    assert log.count(marker) == (0 if mode == "fix" else count), f"Wrong diagnostic count for {marker}\n{log}"
merge = (root / "merge.log").read_text()
merge_status = int((root / "merge-exit-code").read_text())
assert merge_status == (0 if mode == "fix" else 1), f"Unexpected merge exit status {merge_status}\n{merge}"
assert re.search(r"^--- " + ("PASS" if mode == "fix" else "FAIL") + r": TestRouteMergeCollisionSuite ", merge, re.M), merge
if mode == "fix":
    upstream = (root / "upstream-tests.log").read_text()
    assert "--- PASS: TestRouteSpecSuite " in upstream and "--- FAIL:" not in upstream, upstream
    assert len(re.findall(r"^    --- PASS: TestRouteSpecSuite/", upstream, re.M)) == 8, upstream
    assert "--- PASS: TestRouteMergeSuite " in upstream, upstream
    print("PASS: all nine route regressions, the kernel-key merge regression, and existing route/merge suites passed.")
else:
    print("REPRODUCED: RA collision, transition, merge, interface-ownership and stale-delete defects; three controls passed.")
