{
  description = "autogit: keep git repositories in sync with their remotes";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "autogit";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-qnw/KfRB6BgjNLIcpTNmfEtGCmkaZnGf0fW9sKq0ebQ=";
          nativeBuildInputs = [ pkgs.makeWrapper ];
          nativeCheckInputs = [ pkgs.git ];
          # Fall back to these when PATH (e.g. under systemd) lacks them.
          postInstall = ''
            wrapProgram $out/bin/autogit \
              --suffix PATH : ${pkgs.lib.makeBinPath [ pkgs.git pkgs.openssh ]}
          '';
          meta.mainProgram = "autogit";
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls pkgs.git ]; };
      });
    };
}
