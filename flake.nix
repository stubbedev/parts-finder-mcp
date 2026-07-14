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
          version = "0.1.14";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that
          # hash so the sandboxed build is reproducible. Kept in sync
          # with go.sum by `just sync-flake` (and CI auto-bump).
          # go-sum: 231b2fe11b3a5a4871202cab8913ac8741d8125482edeb2d8881ef135b8f5539
          vendorHash = "sha256-/V7KtWNQNxtWhJk4oby9zZFzPNU0uJ4rax9DGg9BayY=";
          # Unit tests hit the network-free paths only, but keep the
          # sandbox check fast and deterministic: vet+tests run in CI.
          doCheck = false;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=0.1.14"
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
