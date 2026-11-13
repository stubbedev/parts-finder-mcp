{
  perSystem = { inputs', lib, pkgs, self', ... }: {
    packages = {
      default = self'.packages.parts-finder;
      parts-finder = (pkgs.buildGoModule.override { go = inputs'.devtools.packages.go; }) rec {
        pname = "parts-finder";
        version = "0.1.20";
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
        # go-sum: f51c0905734b73fd0a120194158050f106b46da40a4c8ba155ed7bc5a3a54a01
        vendorHash = "sha256-WL3oi8tPW35eNVoDAVOFpgHsH9Sb/byekr5c/vhnCRk=";
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
