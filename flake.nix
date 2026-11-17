{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      forAll = f: nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (system:
        f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "caldav";
          version = "1.0.0";
          src = ./.;
          vendorHash = "sha256-yESw9Xu1K81wmHDs7ij4jjTQm5lHuv0wwL4dyOcFFk4=";
          # the Turso driver extracts its native library to a cache dir at runtime
          preCheck = "export TURSO_GO_CACHE_DIR=$TMPDIR";
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go ];
        };
      });
    };
}
