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
      in
      {
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
      }
    );
}

