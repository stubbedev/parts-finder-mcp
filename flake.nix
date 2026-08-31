{
  description = "parts-finder-mcp — MCP server for speccing servers from compatible hardware";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        parts-finder = pkgs.buildGoModule {
          pname = "parts-finder";
          version = "0.1.18";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that
          # hash so the sandboxed build is reproducible. Kept in sync
          # with go.sum by `just sync-flake` (and CI auto-bump).
          # go-sum: d16691072be0488bd223e86ace25b6968b98639500e063c74942c59fcffc080f
          vendorHash = "sha256-itSmzsYgl1DJ+tGBbSXe/Q6dwWuctaRfhHCcD4uqyQ8=";
          # Unit tests hit the network-free paths only, but keep the
          # sandbox check fast and deterministic: vet+tests run in CI.
          doCheck = false;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=0.1.18"
          ];
        };
      in
      {
        packages = {
          default = parts-finder;
          parts-finder = parts-finder;
        };

        apps.default = flake-utils.lib.mkApp {
          drv = parts-finder;
          name = "parts-finder";
        };

        checks.build = parts-finder;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            just
            git
          ];
          shellHook = ''
            echo "parts-finder-mcp dev shell — \`just build\` to compile, \`just test\` to test"
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
