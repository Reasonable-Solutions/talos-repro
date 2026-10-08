{
  description = "Talos IPv6 route collision and transition reproduction with a verified patch";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/a0374025a863d007d98e3297f6aa46cc3141c2f0";
    toolchain.url = "github:NixOS/nixpkgs/0e251e24a4f24e036a084b6b4b2d2491af4167f4";
  };
  outputs =
    { nixpkgs, toolchain, ... }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      go = toolchain.legacyPackages.${system}.go;
      revision = "2f86b9d2a29b413deddd7122a8420b8913813615";
      source = pkgs.fetchurl {
        name = "talos-${revision}.tar.gz";
        url = "https://api.github.com/repos/siderolabs/talos/tarball/${revision}";
        hash = "sha256-Nb1VWFsCApOD5it7l3in+Uye8hbAZ+vWXlhKH+s6y1g=";
      };
      mkBinary =
        patched:
        (pkgs.buildGoModule.override { inherit go; }) {
          pname = "talos-route-tests-${if patched then "fix" else "repro"}";
          version = "1.14.1";
          src = source;
          vendorHash = "sha256-WlLh9AVDWI0Z/Vp9rCJuhvtU5qJIDYZ463sg0AQIY7E=";
          overrideModAttrs = _: _: { name = "talos-${revision}-go-modules"; };
          postPatch = "rm -f go.work go.work.sum";
          # Keep the dependency FOD identical for stock and patched source.
          postConfigure =
            pkgs.lib.optionalString patched ''
              patch -p1 < ${./route-reconciliation.patch}
            ''
            + ''
              cp ${./route_regression_test.go} internal/app/machined/pkg/controllers/network/route_repro_test.go
            '';
          env.CGO_ENABLED = 0;
          buildPhase = ''
            runHook preBuild
            go test -c -o route-tests ./internal/app/machined/pkg/controllers/network
            runHook postBuild
          '';
          doCheck = true;
          checkPhase = ''
            go test -v ./internal/app/machined/pkg/controllers/network/internal/bgp -count=1 > source-tests.log 2>&1 || {
              cat source-tests.log
              exit 1
            }
          '';
          installPhase = ''
            mkdir -p $out/bin
            cp route-tests $out/bin/
            cp source-tests.log $out/
          '';
        };
      mkCase =
        patched:
        let
          name = if patched then "fix" else "repro";
          binary = mkBinary patched;
          upstreamRunner = pkgs.writeShellScript "route-upstream-tests" ''
            ${pkgs.iproute2}/bin/ip link set lo up
            exec ${binary}/bin/route-tests -test.run '^TestRouteSpecSuite$' -test.v
          '';
          vm = pkgs.testers.runNixOSTest {
            name = "talos-route-${name}";
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
              status, _ = machine.execute(f"unshare --net env TALOS_ROUTE_REPRO_NETNS=1 TALOS_ROUTE_REPRO_PARENT_NETNS={parent_netns} ${binary}/bin/route-tests -test.run '^TestRouteRegression$' -test.v > /tmp/regression.log 2>&1")
              machine.succeed(f"echo {status} > /tmp/exit-code")
              machine.copy_from_vm("/tmp/regression.log")
              machine.copy_from_vm("/tmp/exit-code")
              ${pkgs.lib.optionalString patched ''
                machine.succeed("unshare --net ${upstreamRunner} > /tmp/upstream-tests.log 2>&1")
                machine.copy_from_vm("/tmp/upstream-tests.log")
              ''}
            '';
          };
        in
        pkgs.runCommand "talos-route-${name}" { nativeBuildInputs = [ pkgs.python3 ]; } ''
          mkdir -p $out/bin
          cp ${vm}/regression.log ${vm}/exit-code $out/
          ${pkgs.lib.optionalString patched "cp ${vm}/upstream-tests.log $out/"}
          cp ${binary}/source-tests.log $out/
          python ${./check-result.py} ${name} $out > $out/result
          cat $out/result
          cp ${./route_regression_test.go} $out/route_regression_test.go
          cp ${./route-reconciliation.patch} $out/route-reconciliation.patch
          ln -s ${binary}/bin/route-tests $out/bin/route-tests
        '';
      cases = {
        repro = mkCase false;
        fix = mkCase true;
      };
    in
    assert go.version == "1.26.5";
    {
      packages.${system} = cases // {
        default = cases.fix;
        repro-tests = mkBinary false;
        fix-tests = mkBinary true;
      };
      checks.${system} = cases // {
        validator =
          pkgs.runCommand "talos-route-validator-check" { nativeBuildInputs = [ pkgs.python3 ]; }
            ''
              python ${./test_validator.py} ${./check-result.py} ${cases.repro} ${cases.fix} > $out
            '';
      };
      formatter.${system} = pkgs.nixfmt;
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          go
          pkgs.nixfmt
          pkgs.python3
          pkgs.iproute2
          pkgs.util-linux
        ];
      };
    };
}
