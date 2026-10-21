{
  perSystem = { lib, pkgs, self', ... }: {
    packages = {
      default = self'.packages.parts-finder;
      parts-finder = pkgs.buildGo127Module rec {
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
        # go-sum: 5c26afcd5421790c7a6421553da559e6400b43e8c55febe6eba3db9ed1ba2378
        vendorHash = "sha256-aMUvzSLCobhahKzzsr+/rKfHxZ4jtg5Tjdztsx3jaPk=";
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
