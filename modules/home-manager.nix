flake:
{ config, lib, pkgs, ... }:

let
  cfg = config.programs.cosmonaut;

  repoName = repository: builtins.elemAt (lib.splitString "/" repository) 1;

  targetModule = { config, lib, ... }: {
    options = {
      repository = lib.mkOption {
        type = lib.types.str;
        description = "GitHub repository in owner/repo form.";
      };

      branch = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Preferred branch when creating or matching a codespace.";
      };

      displayName = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Exact display name to disambiguate matches.";
      };

      codespaceName = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Exact codespace name for strict reuse.";
      };

      workspacePath = lib.mkOption {
        type = lib.types.str;
        default = "/workspaces/${repoName config.repository}";
        description = "Remote folder Zed should open. Defaults to /workspaces/<repo-name>.";
      };

      machine = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Machine type forwarded to gh codespace create.";
      };

      location = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Location forwarded to gh codespace create.";
      };

      devcontainerPath = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Dev container config path forwarded to gh codespace create.";
      };

      idleTimeout = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Idle timeout forwarded to gh codespace create.";
      };

      retentionPeriod = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Retention period forwarded to gh codespace create.";
      };

      uploadBinaryOverSsh = lib.mkOption {
        type = lib.types.nullOr lib.types.bool;
        default = null;
        description = "Set Zed's upload_binary_over_ssh for this host.";
      };

      zedNickname = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Friendly name shown in Zed's remote project list.";
      };

      autoStop = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Auto-stop codespace after idle duration (e.g. 30m).";
      };

      preWarm = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Time-of-day to pre-warm codespace (e.g. 08:00).";
      };

      coder = lib.mkOption {
        type = lib.types.nullOr (lib.types.submodule ({ ... }: {
          options = {
            template = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              description = "Coder template name used to create the workspace.";
            };

            workspaceName = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              description = "Exact Coder workspace name for reuse and creation.";
            };

            parameters = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              description = "Coder template parameters passed as --parameter name=value.";
            };

            stopAfter = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              description = "Auto-stop duration passed to coder create (for example 8h).";
            };

            organization = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              description = "Override the default Coder organization for this target.";
            };

            portForwards = lib.mkOption {
              type = lib.types.listOf (lib.types.submodule ({ ... }: {
                options = {
                  label = lib.mkOption {
                    type = lib.types.nullOr lib.types.str;
                    default = null;
                    description = "Friendly label shown for this Coder port forward.";
                  };
                  localPort = lib.mkOption {
                    type = lib.types.nullOr lib.types.int;
                    default = null;
                    description = "Localhost port to bind. Defaults to remotePort when omitted.";
                  };
                  remotePort = lib.mkOption {
                    type = lib.types.int;
                    description = "Port inside the Coder workspace.";
                  };
                  protocol = lib.mkOption {
                    type = lib.types.nullOr (lib.types.enum [ "tcp" "udp" ]);
                    default = null;
                    description = "Protocol to forward. Defaults to tcp.";
                  };
                };
              }));
              default = [ ];
              description = "Coder ports exposed in the applet via coder port-forward.";
            };
          };
        }));
        default = null;
        description = "Coder-specific target settings.";
      };
    };
  };

  filterNulls = lib.filterAttrs (_: v: v != null);

  configJSON = builtins.toJSON (filterNulls {
    defaultTarget = cfg.defaultTarget;
    workspaceProvider = cfg.workspaceProvider;
    editor = cfg.editor;
    ssh =
      if cfg.ssh.multiplexer == null
      then null
      else { multiplexer = cfg.ssh.multiplexer; };
    providers = lib.filterAttrs (_: v: v != { }) {
      github = filterNulls {
        enabled = cfg.providers.github.enable;
      };
      coder = filterNulls {
        enabled = cfg.providers.coder.enable;
        organization = cfg.providers.coder.organization;
      };
    };
    targets = lib.mapAttrs (_: target: filterNulls {
      inherit (target)
        repository branch displayName codespaceName workspacePath
        machine location devcontainerPath idleTimeout retentionPeriod
        uploadBinaryOverSsh zedNickname autoStop preWarm;
      coder = if target.coder == null then null else filterNulls {
        inherit (target.coder) template workspaceName stopAfter organization;
        parameters = if target.coder.parameters == { } then null else target.coder.parameters;
        portForwards = if target.coder.portForwards == [ ] then null else target.coder.portForwards;
      };
    }) cfg.targets;
    daemon = lib.optionalAttrs cfg.daemon.enable (filterNulls {
      hotkey = cfg.daemon.hotkey;
      terminal = cfg.daemon.terminal;
      pollInterval = cfg.daemon.pollInterval;
      inhibitSleep = cfg.daemon.inhibitSleep;
    });
  });

  configFile = pkgs.writeText "cosmonaut-config.json" configJSON;

  wrappedPackage = pkgs.symlinkJoin {
    name = "cosmonaut-wrapped";
    paths = [ cfg.package ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
    postBuild = ''
      wrapProgram $out/bin/cosmonaut \
        --add-flags "--config ${configFile}"
    '';
  };
in
{
  options.programs.cosmonaut = {
    enable = lib.mkEnableOption "cosmonaut launcher";

    package = lib.mkOption {
      type = lib.types.package;
      default = flake.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "The cosmonaut package to use.";
    };

    defaultTarget = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Default target name when none is specified on the command line.";
    };

    editor = lib.mkOption {
      type = lib.types.nullOr (lib.types.enum [ "zed" "neovim" ]);
      default = null;
      description = "Editor to use for opening codespaces (zed or neovim). Defaults to zed.";
    };

    workspaceProvider = lib.mkOption {
      type = lib.types.nullOr (lib.types.enum [ "github" "coder" ]);
      default = null;
      description = "Workspace provider to use globally. Defaults to github.";
    };

    providers.github.enable = lib.mkOption {
      type = lib.types.nullOr lib.types.bool;
      default = null;
      description = ''
        Whether the GitHub Codespaces provider is enabled. null (the
        default) leaves it on; false hides its tray/GUI sections,
        auth prompts, and health checks, and stops polling gh.
      '';
    };

    providers.coder.enable = lib.mkOption {
      type = lib.types.nullOr lib.types.bool;
      default = null;
      description = ''
        Whether the Coder provider is enabled. null (the default)
        leaves it on; false hides its tray/GUI sections and stops
        polling the coder CLI.
      '';
    };

    providers.coder.organization = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Default Coder organization name or UUID.";
    };

    ssh.multiplexer = lib.mkOption {
      type = lib.types.nullOr (lib.types.enum [ "none" "tmux" "zellij" ]);
      default = null;
      description = ''
        Terminal multiplexer that `cosmonaut shell` and the GUI/TUI SSH
        buttons attach to on the remote, so the session survives SSH
        drops. Applies to every workspace unless overridden per
        workspace in the app. null (the default) means none.
      '';
    };

    targets = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule targetModule);
      default = { };
      description = "Named codespace targets.";
    };

    daemon = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Whether to enable the cosmonaut background daemon (tray, hotkey, lifecycle).";
      };

      hotkey = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = if pkgs.stdenv.hostPlatform.isDarwin then "Cmd+Shift+S" else "Ctrl+Shift+S";
        description = "Global hotkey to open the codespace picker.";
      };

      terminal = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Terminal application for launching the picker (null for auto-detect).";
      };

      pollInterval = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = "5m";
        description = "Interval for polling codespace states.";
      };

      inhibitSleep = lib.mkOption {
        type = lib.types.enum [ "off" "sleep" "sleep+shutdown" ];
        default = "off";
        description = ''
          Hold a sleep/shutdown inhibitor while a codespace session is active:
          - "off" (default): never inhibit
          - "sleep": inhibit idle sleep while any launched SSH session is alive
          - "sleep+shutdown": also inhibit shutdown (Linux only; on macOS this
            degrades to "sleep" because there is no user-space shutdown inhibitor)
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ wrappedPackage ];

    programs.ssh.includes = [ "~/.ssh/cosmonaut/*.conf" ];

    # Warn (don't fail) when programs.ssh would render a bare `Host *`
    # rule into ~/.ssh/config. Such a rule matches codespace hosts
    # (cs.* / cs-*) and, when paired with an IdentityFile pointing at a
    # YubiKey/SK key, breaks `gh codespace ssh` whenever the device is
    # unplugged. The same check runs at runtime via `cosmonaut doctor`.
    warnings =
      let
        ssh = config.programs.ssh;
        matchBlocksKeys = lib.attrNames (ssh.matchBlocks or { });
        # matchBlocks key is a literal Host pattern, so a key of "*"
        # alone is the unscoped catch-all. Keys like "* !cs-* !cs.*"
        # already exclude codespace hosts and are fine.
        bareStarMatchBlock = lib.any (k: k == "*") matchBlocksKeys;
        # extraConfig: any line that is exactly `Host *` (canonical case,
        # optional leading whitespace) is a bare catch-all.
        bareStarLineRe = "[[:space:]]*Host[[:space:]]+\\*[[:space:]]*";
        extraConfigLines = lib.splitString "\n" (ssh.extraConfig or "");
        bareStarInExtraConfig =
          lib.any (l: builtins.match bareStarLineRe l != null) extraConfigLines;
        msg = ''
          cosmonaut: programs.ssh has a bare `Host *` rule that also matches
          codespace hosts (cs.* and cs-*). When that block sets an IdentityFile
          pointing at a YubiKey/SK key, `gh codespace ssh` and cosmonaut's
          editor launch will fail when the device is unplugged.

          Fix: replace the catch-all pattern with `* !cs-* !cs.*` so codespace
          hosts skip the rule. For matchBlocks, change the attribute key:

              matchBlocks."* !cs-* !cs.*" = { ... };

          For extraConfig, change the Host line:

              Host * !cs-* !cs.*

          Run `cosmonaut doctor` to re-check at runtime.
        '';
      in
      lib.optional (bareStarMatchBlock || bareStarInExtraConfig) msg;

    home.file.".ssh/cosmonaut/.keep".text = "";

    # Copy .app bundle into ~/Applications on activation.
    # home.file with recursive=true creates per-file symlinks which macOS
    # does not recognise as a valid bundle. Instead, copy the whole .app
    # directory and strip the quarantine xattr so Gatekeeper accepts it.
    home.activation.cosmonaut-app = lib.mkIf pkgs.stdenv.hostPlatform.isDarwin
      (lib.hm.dag.entryAfter [ "writeBoundary" ] ''
        app_src="${wrappedPackage}/Applications/Cosmonaut.app"
        app_dst="$HOME/Applications/Cosmonaut.app"
        $DRY_RUN_CMD rm -rf "$app_dst"
        $DRY_RUN_CMD cp -RL "$app_src" "$app_dst"
        $DRY_RUN_CMD chmod -R u+w "$app_dst"
        $DRY_RUN_CMD xattr -dr com.apple.quarantine "$app_dst" 2>/dev/null || true
      '');

    # macOS launchd agent for the daemon.
    launchd.agents.cosmonaut-daemon = lib.mkIf (cfg.daemon.enable && pkgs.stdenv.hostPlatform.isDarwin) {
      enable = true;
      config = {
        # Launch from the .app bundle so macOS associates the process
        # with the bundle (dock icon, app lifecycle, permissions).
        # The .app binary only has the gh PATH wrapper: it doesn't
        # include the home-manager --config wrapper, so pass it here.
        ProgramArguments = [
          "${wrappedPackage}/Applications/Cosmonaut.app/Contents/MacOS/cosmonaut"
          "--config" "${configFile}"
          "applet"
        ];
        # Only restart on abnormal exit: lets the user quit cleanly
        # via the tray menu without launchd immediately restarting.
        KeepAlive = { SuccessfulExit = false; };
        RunAtLoad = true;
        Label = "com.cosmonaut.daemon";
        StandardOutPath = "${config.home.homeDirectory}/Library/Logs/cosmonaut.log";
        StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/cosmonaut.log";
        ProcessType = "Interactive";
      };
    };

    # Linux systemd user service for the daemon.
    systemd.user.services.cosmonaut-daemon = lib.mkIf (cfg.daemon.enable && pkgs.stdenv.hostPlatform.isLinux) {
      Unit = {
        Description = "cosmonaut background daemon";
        After = [ "graphical-session.target" ];
        PartOf = [ "graphical-session.target" ];
      };
      Service = {
        ExecStart = "${wrappedPackage}/bin/cosmonaut applet";
        Restart = "on-failure";
        RestartSec = 5;
      };
      Install = {
        WantedBy = [ "graphical-session.target" ];
      };
    };
  };
}
