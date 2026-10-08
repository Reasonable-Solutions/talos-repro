# Talos IPv6 route collisions can stall route reconciliation

A BGP default route can collide with a router-advertisement (RA) default in
Talos's Linux route table. Route replacement and cleanup can also fail when
next-hops move between interfaces. These errors abort `RouteSpecController`;
the controller runtime then backs off the entire controller, delaying unrelated
route changes even after BGP has re-established its session.

This project provides **the same regression tests against stock and patched
Talos**, a source patch, and Nix `repro` / `fix` outputs that run the tests against
a real Linux kernel in a small NixOS VM.

Pinned source: **Talos 1.14.1**, commit
[`2f86b9d2a29b413deddd7122a8420b8913813615`](https://github.com/siderolabs/talos/tree/2f86b9d2a29b413deddd7122a8420b8913813615).
The flake pins the unpacked source tree, vendored dependencies, Go **1.26.5**, and the
NixOS test environment. Other Talos versions have not been evaluated here.

## Why we are fixing this

We found the problem in an IPv6-only, unnumbered eBGP fabric with two SONiC
switches and Talos nodes connected through independent uplinks. Nodes retain a
loopback identity when cables move; router advertisements support bootstrap,
while BGP supplies routes after enrollment.

In a retained failure, the replacement BGP peer was established at **11:27:00**,
but replacement kernel routes were not installed until **11:29:34**. Talos
logged repeated `EEXIST` errors for the default route, followed by `ESRCH`
cleanup errors during the move. The 30-second recovery test failed.

This is separate from the GoBGP patch that preserves received IPv6 link-local
next hops. The problem remained with that patch installed. This reproducer
constructs learned route specifications using Talos's own `bgp.RouteSpec`
helper; it does not parse BGP updates or apply the GoBGP patch.

## What goes wrong

The common [regression test](route_regression_test.go) covers eight cases:

| Case | Expected behavior | Stock pinned Talos |
| --- | --- | --- |
| RA control | The stock controller installs a BGP spec explicitly set to metric 100 beside RA at 1024 | Passes |
| IPv4 metric control | Learned IPv4 BGP routes keep metric zero | Passes |
| IPv4 link move | A running IPv4 route moves between links while retaining its RouteID | Passes |
| BGP beside RA | A learned BGP default coexists with an RA default on the other link | `EEXIST` |
| Next-hop replacement | Remove a withdrawn next-hop before adding its replacement, regardless of resource order | `EEXIST` |
| Interface ownership | Cleanup on one interface preserves a route through the same link-local gateway on another | Deletes the other interface's route |
| Missing-interface ownership | Cleanup for a disappeared interface preserves the surviving interface's route | Deletes the surviving route |
| Idempotent deletion | A route removed after the kernel snapshot counts as already cleaned up | `ESRCH` |

Talos's [`bgp.RouteSpec`](https://github.com/siderolabs/talos/blob/2f86b9d2a29b413deddd7122a8420b8913813615/internal/app/machined/pkg/controllers/network/internal/bgp/bgp.go)
leaves the route priority at zero. Linux normalizes this to metric **1024** for
IPv6, the same metric used by the RA defaults in this setup. Talos's exclusive
route-add operation then conflicts with a default through another gateway.

The pinned [`RouteSpecController`](https://github.com/siderolabs/talos/blob/2f86b9d2a29b413deddd7122a8420b8913813615/internal/app/machined/pkg/controllers/network/route_spec.go)
uses a shared kernel snapshot while processing the resource list in its given
order. A new route can be attempted before the withdrawn route is removed.
Matching also omits the requested output interface, although identical
link-local gateways can exist on different links. Finally, an already-absent
route is treated as a failed deletion.

Each condition can return an error from the controller. The regression tests
exercise a single reconciliation pass rather than depending on a flaky timing
assertion about the subsequent exponential backoff.

## The patch and why it helps

[route-reconciliation.patch](route-reconciliation.patch) changes only two Talos
implementation files. Tests are separate so the **identical test source compiles
and runs before and after applying the patch**.

The patch:

1. Assigns learned **IPv6** BGP routes metric **100**, keeping them distinct from
   RA defaults at 1024 and preferring BGP while preserving RA fallback. IPv4
   BGP routes retain metric zero.
2. Processes withdrawals before additions and refreshes the kernel snapshot
   between those phases. A replacement cannot be mistaken for, or collide with,
   a route that should have been removed in the same pass.
3. Includes the requested output interface in **IPv6** route matching, following
   Talos `RouteID` identity. Cleanup for a missing interface cannot match the
   surviving interface's IPv6 route. IPv4 matching remains independent of the
   interface so a running spec can delete its old-link route and install its
   replacement on another link.
4. Treats only `ESRCH` from deletion as successful cleanup. Other errors still
   propagate; the patch does not suppress arbitrary `EEXIST` errors.

Metric 100 is a deliberate routing-policy choice for this fabric. Upstream
review should decide whether it should be the general default or configurable,
and whether to split that policy change from the controller corrections.
The existing route-controller suite passes with the patch. The RA control
runs through that controller with an explicit priority of 100, showing that
the metric choice alone resolves the coexistence case.

Review of the first patch found an IPv4 regression: filtering every family by
interface prevented replacement of an existing IPv4 route after its output
link changed. The new `IPv4LinkMove` test passes on stock, fails on the first
patch with `EEXIST`, and must pass on the corrected patch. `IPv4MetricControl`
also rejects the first patch's unintended IPv4 metric change. These controls
remain mandatory in both `repro` and `fix`.

## Reproduce and verify with Nix

Use **x86_64 Linux**, Nix with flakes, and a builder supporting NixOS VM tests
and KVM. The caller does not need to run Nix as root; the Nix builder needs
access to `/dev/kvm`. Run from this checkout:

```sh
nix build .#repro -o result-repro -L
cat result-repro/result
cat result-repro/regression.log

nix build .#fix -o result-fix -L
cat result-fix/result
cat result-fix/regression.log
cat result-fix/upstream-tests.log

nix flake check -L
```

**A successful `repro` build means the expected bugs were reproduced.** The
stock test process must exit 1, all three controls must pass, and exactly the five
specified subtests must fail with their expected diagnostics. Compiler errors,
skips, panics, setup failures and unrelated assertions do not count. The
[output validator](check-result.py) checks the exit status, exact subtest results
and diagnostic counts. `nix flake check` also tests the validator against
valid logs and malformed, skipped, unrelated or incomplete results.

Expected `repro` result:

```text
REPRODUCED: RA collision, add-before-withdrawal, wrong-interface cleanup and stale deletion; three controls passed.
```

The `fix` output requires all eight common regression cases and all eight existing
`TestRouteSpecSuite` cases to pass:

```text
PASS: all eight route regressions and eight existing route-controller tests passed.
```

Both builds also run Talos's existing internal BGP unit tests. Outputs retain
`regression.log`, `exit-code`, `source-tests.log`, the test source, the patch and
`bin/route-tests`; `fix` additionally retains `upstream-tests.log`. The default
package is `fix`. These are test artifacts, not Talos boot/installer images.
Initial builds fetch pinned inputs. Tests need no external network service.

### Run without a VM

The compiled tests can also run in a disposable user/network namespace on a
Linux host that permits unprivileged namespaces. This uses the host's kernel
rather than the flake's pinned VM kernel:

```sh
nix build .#repro-tests -o stock-tests
nix build .#fix-tests -o fixed-tests

# Record this before entering the disposable namespace.
export TALOS_ROUTE_REPRO_PARENT_NETNS="$(readlink /proc/self/ns/net)"

# Expected exit 1: three controls pass; the five defect cases fail.
unshare --user --map-root-user --net env TALOS_ROUTE_REPRO_NETNS=1 \
  ./stock-tests/bin/route-tests -test.run '^TestRouteRegression$' -test.v

# Expected exit 0: all eight pass.
unshare --user --map-root-user --net env TALOS_ROUTE_REPRO_NETNS=1 \
  ./fixed-tests/bin/route-tests -test.run '^TestRouteRegression$' -test.v
```

The test refuses the recorded parent network namespace and refuses to silently skip
when permissions are missing. It creates temporary dummy links and default
routes only inside the disposable namespace.

### Use an existing Talos checkout

With the pinned Talos source and an appropriate Go toolchain/dependencies:

```sh
repro_dir=/path/to/talos-route-repro
export TALOS_ROUTE_REPRO_PARENT_NETNS="$(readlink /proc/self/ns/net)"
cp "$repro_dir/route_regression_test.go" \
  internal/app/machined/pkg/controllers/network/route_repro_test.go

GOWORK=off go test -c -o /tmp/talos-route-tests \
  ./internal/app/machined/pkg/controllers/network
unshare --user --map-root-user --net env TALOS_ROUTE_REPRO_NETNS=1 \
  /tmp/talos-route-tests -test.run '^TestRouteRegression$' -test.v

patch -p1 < "$repro_dir/route-reconciliation.patch"
GOWORK=off go test -c -o /tmp/talos-route-tests \
  ./internal/app/machined/pkg/controllers/network
unshare --user --map-root-user --net env TALOS_ROUTE_REPRO_NETNS=1 \
  /tmp/talos-route-tests -test.run '^TestRouteRegression$' -test.v
```

## Evidence and scope

The resource store/event source is stubbed to feed a deterministic ordering
into the actual Talos controller. Route creation, listing and deletion use
**real rtnetlink calls and the Linux kernel**. The RA control installs a route
labelled `proto ra`; it does not emulate an RA daemon. The standalone VM is
NixOS running a Talos test binary, not a booted Talos cluster. No SONiC or BGP
session is needed to reproduce these controller defects.

Separately, the corrected implementation patch was included in custom Talos
`1.14.1-sokk.4` and qualified in a KVM fabric containing two routers, two SONiC
VS switches, three control planes and three workers. All **41 cases passed**:
six upgrades from the original baseline, the full fault matrix on the patched
candidate, and six rollbacks with persistent data, configuration, identities,
etcd quorum and running binary hashes checked. Cable move-and-return took
16.4 seconds; the longest sampled ingress outage was 16.2 seconds, within the
unchanged 30-second budget. This cluster qualification is separate from the
standalone flake.

Post-fault logs on one worker nevertheless contained **three transient IPv6 BGP
`EEXIST` controller failures** at metric 100 during power recovery.
Reconciliation recovered within the test budget. The resource/kernel ordering
behind these remaining collisions has not yet been reproduced deterministically.
The tests here cover the eight listed cases; they do **not** establish that all
route-controller failures during asynchronous BGP churn are eliminated.

Physical switch ASIC behavior and different Talos versions remain outside this
reproduction. No upstream report or pull request has been submitted by this
project's preparation.
