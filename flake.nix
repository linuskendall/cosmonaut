{
  description = "Cosmonaut Launcher – start/create GitHub Codespaces and open them in Zed";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    nix-appimage = {
      url = "github:ralismark/nix-appimage";
      inputs.nixpkgs.follows = "nixpkgs";
      # Removed flake-utils follow; ralismark/nix-appimage doesn't use it.
    };
  };

  outputs = { self, nixpkgs, flake-utils, nix-appimage }:
    let
      release = {
        owner = "linuskendall";
        repo = "cosmonaut";
        tag = "v0.0.0-placeholder";
        linuxSha = "0000000000000000000000000000000000000000000000000000";
        darwinSha = "0000000000000000000000000000000000000000000000000000";
      };
    in
    {
      homeManagerModules.default = import ./modules/home-manager.nix self;
      homeManagerModules.cosmonaut = import ./modules/home-manager.nix self;
      homeManagerModules.codespace-zed = import ./modules/home-manager.nix self;
    }
    //
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        cgoLinuxLibs = pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
          pkgs.gtk3
          pkgs.libappindicator-gtk3
          pkgs.libGL
          pkgs.xorg.libX11
          pkgs.xorg.libXcursor
          pkgs.xorg.libXi
          pkgs.xorg.libXinerama
          pkgs.xorg.libXrandr
          pkgs.xorg.libXxf86vm
          pkgs.xorg.libXext
          pkgs.xorg.libXfixes
          pkgs.xorg.libXrender
          pkgs.xorg.xorgproto
        ];

        cgoLinuxPkgConfigPath =
          pkgs.lib.makeSearchPathOutput "dev" "lib/pkgconfig" cgoLinuxLibs;
        cgoLinuxCFLAGS = builtins.concatStringsSep " "
          (map (lib: "-isystem ${pkgs.lib.getDev lib}/include") cgoLinuxLibs);
        cgoLinuxLDFLAGS = builtins.concatStringsSep " "
          (map (lib: "-L${pkgs.lib.getLib lib}/lib") cgoLinuxLibs);

        cosmonautFromSource = pkgs.buildGoModule {
          pname = "cosmonaut";
          version = "unstable";
          src = ./.;

          vendorHash = "sha256-lXAVp3yJWoT/sok4o5GsBIFgKvLRD0KQj40R47CT5XQ=";

          env.CGO_ENABLED = 1;
          tags = [ "netgo" ];

          # cctools ld (the darwin stdenv default) segfaults linking the
          # fyne/objc-heavy binary against nixpkgs >= 26.11's clang 21 / Go 1.26
          # toolchain. lld links it fine.
          ldflags = pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [
            "-extldflags=-fuse-ld=lld"
          ];

          nativeBuildInputs = [
            pkgs.makeWrapper
            pkgs.installShellFiles
            pkgs.pkg-config
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            pkgs.xvfb-run
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [
            pkgs.lld
          ];

          checkPhase = ''
            runHook preCheck
            export GOFLAGS=''${GOFLAGS//-trimpath/}
            ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux "xvfb-run -a "}go test -tags=netgo ${
              pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''-ldflags="-extldflags=-fuse-ld=lld" ''
            }./...
            runHook postCheck
          '';

          buildInputs = pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [
            pkgs.apple-sdk
          ] ++ cgoLinuxLibs;

          postInstall = ''
            wrapProgram $out/bin/cosmonaut \
              --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.gh ]}

            completionCmd="$out/bin/cosmonaut"
            if [[ "$(uname)" == "Linux" ]]; then
              completionCmd="xvfb-run $completionCmd"
            fi
            installShellCompletion --cmd cosmonaut \
              --bash <($completionCmd completion bash) \
              --zsh <($completionCmd completion zsh) \
              --fish <($completionCmd completion fish)

            install -Dm644 $src/dist/cosmonaut.config.example.json $out/share/cosmonaut/cosmonaut.config.example.json
            install -Dm644 $src/dist/cosmonaut.service $out/share/cosmonaut/cosmonaut.service
          '' + pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''
            mkdir -p "$out/Applications/Cosmonaut.app/Contents/MacOS"
            mkdir -p "$out/Applications/Cosmonaut.app/Contents/Resources"
            cp $src/dist/Info.plist "$out/Applications/Cosmonaut.app/Contents/Info.plist"
            cp $src/assets/logo.icns "$out/Applications/Cosmonaut.app/Contents/Resources/icon.icns"
            # The bundle executable MUST be the real Mach-O binary, not the
            # makeWrapper shell script. A GUI applet relies on
            # [[NSBundle mainBundle]] resolving from its running executable's
            # path up to Foo.app/Contents/MacOS. The wrapper re-execs the real
            # binary out of $out/bin/.cosmonaut-wrapped, which lives outside any
            # .app layout, so mainBundle fails to resolve, LSUIElement is
            # ignored, and the applet gets a generic Dock tile named
            # ".cosmonaut-wrapped" instead of staying menu-bar-only.
            # gh reaches the daemon via the login-shell PATH merge at runtime,
            # so the bundle does not need the gh wrapper.
            cp $out/bin/.cosmonaut-wrapped "$out/Applications/Cosmonaut.app/Contents/MacOS/cosmonaut"
          '';

          meta = with pkgs.lib; {
            description = "CLI for starting GitHub Codespaces and opening them in Zed";
            license = licenses.mit;
            mainProgram = "cosmonaut";
          };
        };

        cosmonautPrebuilt = pkgs.stdenvNoCC.mkDerivation rec {
          pname = "cosmonaut-prebuilt";
          version = pkgs.lib.removePrefix "v" release.tag;

          src =
            if pkgs.stdenv.hostPlatform.isLinux then
              pkgs.fetchurl {
                url = "https://github.com/${release.owner}/${release.repo}/releases/download/${release.tag}/cosmonaut-amd64.tar.gz";
                sha256 = release.linuxSha;
              }
            else
              pkgs.fetchurl {
                url = "https://github.com/${release.owner}/${release.repo}/releases/download/${release.tag}/cosmonaut-macos-arm64.tar.gz";
                sha256 = release.darwinSha;
              };

          nativeBuildInputs = [
            pkgs.makeWrapper
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            pkgs.autoPatchelfHook
          ];

          buildInputs = cgoLinuxLibs;

          dontBuild = true;

          installPhase =
            if pkgs.stdenv.hostPlatform.isLinux then ''
              runHook preInstall
              install -Dm755 cosmonaut/cosmonaut $out/bin/cosmonaut
              install -Dm644 cosmonaut/cosmonaut.config.example.json \
                $out/share/cosmonaut/cosmonaut.config.example.json
              install -Dm644 cosmonaut/cosmonaut.service \
                $out/share/cosmonaut/cosmonaut.service
              wrapProgram $out/bin/cosmonaut \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.gh ]}
              runHook postInstall
            '' else ''
              runHook preInstall
              mkdir -p $out/Applications $out/bin
              cp -R Cosmonaut.app $out/Applications/Cosmonaut.app
              ln -s $out/Applications/Cosmonaut.app/Contents/MacOS/cosmonaut $out/bin/cosmonaut
              runHook postInstall
            '';

          meta = with pkgs.lib; {
            description = "Cosmonaut launcher (prebuilt from goreleaser release)";
            homepage = "https://github.com/${release.owner}/${release.repo}";
            license = licenses.mit;
            mainProgram = "cosmonaut";
            platforms = [ "x86_64-linux" "aarch64-darwin" ];
          };
        };

        cgoEnvSetup = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
          export PKG_CONFIG_PATH="${cgoLinuxPkgConfigPath}:''${PKG_CONFIG_PATH:-}"
          export CGO_CFLAGS="${cgoLinuxCFLAGS} ''${CGO_CFLAGS:-}"
          export CGO_LDFLAGS="${cgoLinuxLDFLAGS} ''${CGO_LDFLAGS:-}"
        '';

        cosmonautLint = pkgs.writeShellApplication {
          name = "cosmonaut-lint";
          runtimeInputs = [ pkgs.go pkgs.gofumpt pkgs.golangci-lint ]
            ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
              pkgs.pkg-config
              pkgs.stdenv.cc
            ];
          text = cgoEnvSetup + ''
            unformatted="$(gofumpt -l .)"
            if [ -n "$unformatted" ]; then
              echo "gofumpt found unformatted files:" >&2
              echo "$unformatted" >&2
              echo "Run \`gofumpt -w .\` to fix." >&2
              exit 1
            fi
            golangci-lint run ./...
          '';
        };

        cosmonautTest = pkgs.writeShellApplication {
          name = "cosmonaut-test";
          runtimeInputs = [ pkgs.go ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            pkgs.xvfb-run
            pkgs.pkg-config
            pkgs.stdenv.cc
          ];
          text = cgoEnvSetup + (if pkgs.stdenv.hostPlatform.isLinux then ''
            exec xvfb-run -a go test ./...
          '' else ''
            exec go test ./...
          '');
        };

        cosmonautBuild = pkgs.writeShellApplication {
          name = "cosmonaut-build";
          runtimeInputs = [ cosmonautLint cosmonautTest ];
          text = ''
            cosmonaut-lint
            cosmonaut-test
          '';
        };
      in
      {
        packages = {
          default = cosmonautFromSource;
          cosmonaut = cosmonautFromSource;
        }
        // pkgs.lib.optionalAttrs (system == "x86_64-linux" || system == "aarch64-darwin") {
          cosmonaut-prebuilt = cosmonautPrebuilt;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          appimage = nix-appimage.lib.${system}.mkAppImage {
            # Pointing to wrapped script to keep gh in PATH
            program = "${cosmonautFromSource}/bin/cosmonaut";
            pname = "cosmonaut";
            name = "cosmonaut-${if system == "x86_64-linux" then "x86_64" else "aarch64"}.AppImage";
          };
        };

        apps.lint = {
          type = "app";
          program = "${cosmonautLint}/bin/cosmonaut-lint";
        };

        apps.test = {
          type = "app";
          program = "${cosmonautTest}/bin/cosmonaut-test";
        };

        apps.build = {
          type = "app";
          program = "${cosmonautBuild}/bin/cosmonaut-build";
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gh
            pkgs.pkg-config
            pkgs.golangci-lint
            pkgs.gofumpt
            pkgs.goreleaser
            pkgs.cosign
            pkgs.nix
          ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            pkgs.gtk3
            pkgs.libappindicator-gtk3
            pkgs.libGL
            pkgs.xorg.libX11
            pkgs.xorg.libXcursor
            pkgs.xorg.libXi
            pkgs.xorg.libXinerama
            pkgs.xorg.libXrandr
            pkgs.xorg.libXxf86vm
            pkgs.xorg.libXext
            pkgs.xorg.libXfixes
            pkgs.xvfb-run
          ];

          shellHook = ''
            hook=".git/hooks/pre-commit"
            marker="# cosmonaut-managed"
            if [ -d .git ] && { [ ! -f "$hook" ] || grep -q "$marker" "$hook"; }; then
              mkdir -p .git/hooks
              {
                printf '%s\n' '#!/usr/bin/env bash'
                printf '%s\n' "$marker"
                # Removed EOF block to prevent bash spacing errors caused by Nix indent stripping
                printf '%s\n' 'exec nix run .#lint'
              } > "$hook"
              chmod +x "$hook"
            fi
          '';
        };
      }
    );
}
