{
  perSystem = { inputs', lib, pkgs, self', ... }: {
    packages = {
      default = self'.packages.parts-finder;
      parts-finder = (pkgs.buildGoModule.override { go = inputs'.devtools.packages.go; }) rec {
        pname = "parts-finder";
        version = "0.1.21";
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
        # go-sum: 5c3f97625221c8ecd4539b829fb27e9f50df668781d1d7afdd5b4ae5a429ec5c
        vendorHash = "sha256-t3leR/BTa8CJkLJWaM1ZmfZp1NNwRQOFaViLMl90FuA=";
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
