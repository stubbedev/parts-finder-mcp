{
  perSystem = { pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
      inputsFrom = [ self'.packages.default ];

      packages = with pkgs; [
        gopls
        just
        git
      ];
      shellHook = ''
        echo "parts-finder-mcp dev shell — \`just build\` to compile, \`just test\` to test"
      '';
    };

    formatter = pkgs.nixpkgs-fmt;
  };
}
