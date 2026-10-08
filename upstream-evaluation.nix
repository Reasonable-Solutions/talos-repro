# Experimental backport of #14548; deliberately separate from the qualified fix.
{ pkgs, stockBinary }:
let
  binary = stockBinary.overrideAttrs (_: {
    pname = "talos-route-tests-upstream-14548";
    postConfigure = ''
      patch -p1 < ${./upstream-route-backport.patch}
      # buildGoModule has already vendored the local nested module. RouteID
      # changes must reach that copy as well as the source tree.
      chmod -R u+w vendor/github.com/siderolabs/talos/pkg/machinery
      cp -r pkg/machinery/. vendor/github.com/siderolabs/talos/pkg/machinery/
      cp ${./upstream_regression_test.go} internal/app/machined/pkg/controllers/network/route_repro_test.go
    '';
  });
  runner = pkgs.writeShellScript "upstream-route-suites" ''
    ${pkgs.iproute2}/bin/ip link set lo up
    exec ${binary}/bin/route-tests -test.run '^(TestRouteSpecSuite|TestRouteMergeSuite)$' -test.v
  '';
  vm = pkgs.testers.runNixOSTest {
    name = "talos-upstream-route-evaluation";
    nodes.machine = {
      virtualisation.memorySize = 1024;
      virtualisation.cores = 2;
      boot.kernelModules = [ "dummy" ];
      environment.systemPackages = [
        pkgs.iproute2
        pkgs.util-linux
      ];
    };
    testScript = ''
      machine.start()
      machine.wait_for_unit("multi-user.target")
      parent_netns = machine.succeed("readlink /proc/self/ns/net").strip()
      status, _ = machine.execute(f"unshare --net env TALOS_ROUTE_REPRO_NETNS=1 TALOS_ROUTE_REPRO_PARENT_NETNS='{parent_netns}' ${binary}/bin/route-tests -test.run '^TestUpstreamRouteRegression$' -test.v > /tmp/regression.log 2>&1")
      machine.succeed(f"echo {status} > /tmp/exit-code")
      machine.copy_from_vm("/tmp/regression.log")
      machine.copy_from_vm("/tmp/exit-code")
      machine.succeed("unshare --net ${runner} > /tmp/upstream-tests.log 2>&1")
      machine.copy_from_vm("/tmp/upstream-tests.log")
    '';
  };
in
{
  tests = binary;
  result =
    pkgs.runCommand "talos-upstream-route-evaluation" { nativeBuildInputs = [ pkgs.python3 ]; }
      ''
        mkdir -p $out/bin
        cp ${vm}/regression.log ${vm}/exit-code ${vm}/upstream-tests.log $out/
        cp ${binary}/source-tests.log $out/
        python ${./check-upstream.py} $out > $out/result
        cat $out/result
        ln -s ${binary}/bin/route-tests $out/bin/route-tests
      '';
}
