{
  description = "lightning-compute-protocol dev environment (pinned toolchain + regtest devnet)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      ...
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        lib = pkgs.lib;

        # Prefer Go 1.24+ (go-lcpd/go.mod), but keep the flake evaluatable
        # even if a given nixpkgs revision doesn't carry go_1_24.
        go =
          if pkgs ? go_1_24 then
            pkgs.go_1_24
          else if pkgs ? go_1_24_0 then
            pkgs.go_1_24_0
          else
            pkgs.go;

        nigiriTag = "v0.5.14";
        nigiriPlatform =
          {
            "aarch64-darwin" = {
              os = "darwin";
              arch = "arm64";
              sha256 = "sha256-gWr4XixgTII4W+8KmG9z7kQIYkmlVdCCf7xpROtvUuU=";
            };
            "x86_64-darwin" = {
              os = "darwin";
              arch = "amd64";
              sha256 = "sha256-Y19JWnFgZPNGyw6G9xfoABgv1lyuyl8cuzcQhtCIdf8=";
            };
            "aarch64-linux" = {
              os = "linux";
              arch = "arm64";
              sha256 = "sha256-1tyLtWHUf6PPHauHYq+XOYPtKD6xNW3GkrTpigcPk5k=";
            };
            "x86_64-linux" = {
              os = "linux";
              arch = "amd64";
              sha256 = "sha256-mSwSWLsCzl2+MxKNcerlHZcvGTPl3AvkHEJpGKpJdT8=";
            };
          }
          .${system} or (throw "unsupported system for nigiri: ${system}");

        nigiri = pkgs.stdenvNoCC.mkDerivation {
          pname = "nigiri";
          version = builtins.substring 1 (builtins.stringLength nigiriTag - 1) nigiriTag;
          src = pkgs.fetchurl {
            url = "https://github.com/vulpemventures/nigiri/releases/download/${nigiriTag}/nigiri-${nigiriPlatform.os}-${nigiriPlatform.arch}";
            sha256 = nigiriPlatform.sha256;
          };
          dontUnpack = true;
          installPhase = ''
            runHook preInstall
            mkdir -p "$out/bin"
            cp "$src" "$out/bin/nigiri"
            chmod +x "$out/bin/nigiri"
            runHook postInstall
          '';
          meta = {
            description = "Nigiri Bitcoin development environment (CLI)";
            homepage = "https://github.com/vulpemventures/nigiri";
            mainProgram = "nigiri";
            platforms = lib.platforms.unix;
          };
        };

        ldkLcpNodePackages =
          [
            pkgs.cargo
            pkgs.rustc
            pkgs.perl

            # build.rs uses protoc via tonic-build
            pkgs.protobuf

            # regtest-smoke.sh deps
            pkgs.python3
            pkgs.curl
            nigiri
          ]
          ++ lib.optionals (pkgs ? grpcurl) [ pkgs.grpcurl ]
          ++ lib.optionals (pkgs ? jq) [ pkgs.jq ]
          ++ lib.optionals (pkgs ? pkg-config) [ pkgs.pkg-config ]
          ++ lib.optionals (pkgs ? clang) [ pkgs.clang ]
          ++ lib.optionals (pkgs ? cacert) [ pkgs.cacert ];
      in
      {
        packages.nigiri = nigiri;

        devShells.default = pkgs.mkShell {
          packages =
            [
              go
              pkgs.gnumake
              pkgs.git
              pkgs.perl

              # Go / protobuf toolchain
              pkgs.protobuf
              pkgs.buf
              pkgs.golangci-lint
            ]
            ++ lib.optionals (pkgs ? goimports) [ pkgs.goimports ]
            ++ lib.optionals (pkgs ? golines) [ pkgs.golines ]
            ++ lib.optionals (pkgs ? grpcurl) [ pkgs.grpcurl ]
            ++ lib.optionals (pkgs ? jq) [ pkgs.jq ]

            # Local regtest devnet (Bitcoin Core + LND)
            ++ lib.optionals (pkgs ? bitcoin) [ pkgs.bitcoin ]
            ++ lib.optionals (pkgs ? lnd) [ pkgs.lnd ];

          shellHook = ''
            export LCP_ROOT="${toString ./.}"
            export GOFLAGS="-buildvcs=false"

            echo "devshell ready"
            echo "  test:    make -C go-lcpd test"
            echo "  regtest: make -C go-lcpd test-regtest"
            echo "  devnet:  go-lcpd/scripts/devnet up"
          '';
        };

        devShells."ldk-lcp-node" = pkgs.mkShell {
          packages = ldkLcpNodePackages;

          shellHook = ''
            export LCP_ROOT="${toString ./.}"
            export PATH="${lib.makeBinPath ldkLcpNodePackages}:$PATH"

            echo "ldk-lcp-node devshell ready"
            echo "  build:   cargo build --manifest-path apps/ldk-lcp-node/Cargo.toml"
            echo "  regtest: nigiri start && bash apps/ldk-lcp-node/scripts/regtest-smoke.sh"
          '';
        };
      }
    );
}
