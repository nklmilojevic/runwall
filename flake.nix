{
  description = "Runwall: a live wall of GitHub Actions runs";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs {
        inherit system;
        # The 1Password CLI is unfree; nothing else is.
        config.allowUnfreePredicate = pkg: nixpkgs.lib.getName pkg == "1password-cli";
      }));
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_26
            gopls
            golangci-lint
            just
            lefthook
            oxfmt
            zizmor
            sqlite
            gh
            _1password-cli
            cloudflared
            kubectl
            kustomize
          ] ++ lib.optionals stdenv.hostPlatform.isDarwin [ terminal-notifier ];

          # templ comes from go.mod (`go tool templ`) so local builds and CI use the same version.
          shellHook = ''
            export GOTOOLCHAIN=local
            # Install the git hooks from .lefthook.toml (formatting, zizmor, checks before push).
            if [ -d .git ]; then lefthook install >/dev/null; fi
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
