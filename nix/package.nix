{
  perSystem = { inputs', lib, pkgs, self', ... }: {
    packages = {
      default = self'.packages.parts-finder;
      parts-finder = (pkgs.buildGoModule.override { go = inputs'.devtools.packages.go; }) rec {
        pname = "parts-finder";
        version = "0.1.19";
        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };
        # buildGoModule fetches Go deps through the module proxy and
        # hashes the resulting vendor tree; `vendorHash` pins that
        # hash so the sandboxed build is reproducible. Kept in sync
        # with go.sum by `just sync-flake` (and CI auto-bump).
        # go-sum: 0c0b5bdcee4486b0b00a3042b4fcbcdbe75fa855141c52a1192e0099f6a512cb
        vendorHash = "sha256-rbuAHQ9q9A3taapBvrdqS765AN1brojF5hMt2sSJR/I=";
        # Unit tests hit the network-free paths only, but keep the
        # sandbox check fast and deterministic: vet+tests run in CI.
        doCheck = false;
        ldflags = [
          "-s"
          "-w"
          "-X main.version=${version}"
        ];
        meta.mainProgram = "parts-finder-mcp";
      };
    };

    checks.build = self'.packages.parts-finder;
  };
}
