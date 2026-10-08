"""Prove that unrelated failures cannot masquerade as the expected repro."""
from pathlib import Path
import subprocess
import sys
import tempfile

validator, repro, fixed = map(Path, sys.argv[1:])
checks = 0
for mode, original in (("repro", repro), ("fix", fixed)):
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        originals = {name: (original / name).read_text() for name in
                     ("regression.log", "exit-code", *(["upstream-tests.log"] if mode == "fix" else []))}
        mutations = [None, "compiler", "status", "skip", "extra", "assertion", "ipv4-move", "ipv4-metric"]
        mutations += ["missing-marker", "control"] if mode == "repro" else ["missing-upstream"]
        for mutation in mutations:
            data = dict(originals)
            log = data["regression.log"]
            if mutation == "compiler":
                data["regression.log"] = "FAIL: build failed\n"
            elif mutation == "status":
                data["exit-code"] = "2\n"
            elif mutation == "skip":
                data["regression.log"] = log.replace("--- PASS: TestRouteRegression/RAControl", "--- SKIP: TestRouteRegression/RAControl")
            elif mutation == "extra":
                data["regression.log"] += "    --- FAIL: TestRouteRegression/Unrelated (0.01s)\n"
            elif mutation == "assertion":
                data["regression.log"] += "Error Trace: unrelated assertion failed\n"
            elif mutation in ("ipv4-move", "ipv4-metric"):
                case = "IPv4LinkMove" if mutation == "ipv4-move" else "IPv4MetricControl"
                data["regression.log"] = log.replace(f"--- PASS: TestRouteRegression/{case}", f"--- FAIL: TestRouteRegression/{case}")
            elif mutation == "missing-marker":
                data["regression.log"] = log.replace("REPRO_RA_BGP_COLLISION:", "OTHER:")
            elif mutation == "control":
                data["regression.log"] = log.replace("--- PASS: TestRouteRegression/RAControl", "--- FAIL: TestRouteRegression/RAControl")
            elif mutation == "missing-upstream":
                data["upstream-tests.log"] = "--- PASS: TestRouteSpecSuite (0.01s)\n"
            for name, content in data.items():
                (root / name).write_text(content)
            result = subprocess.run([sys.executable, str(validator), mode, str(root)], capture_output=True)
            assert (result.returncode == 0) == (mutation is None), (mode, mutation, result.stdout, result.stderr)
            checks += 1
print(f"PASS: {checks} validator acceptance/rejection checks")
