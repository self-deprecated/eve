# services.eve for home-manager on the build host (Quartz): runs `eve serve` as a LaunchAgent in
# the user's GUI (Aqua) session — the only place where the login keychain and the Xcode account
# are usable — writes server.yml from Nix settings, and installs the CLI and the agent skill.
# Secrets stay files referenced by path (file:...). Publishing 127.0.0.1:<port> as the svc:eve
# Tailscale Service is the host's job; `tailscaleService` only records whether it should.
self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.eve;
  yaml = pkgs.formats.yaml {};
  configFile = yaml.generate "eve-server.yml" ({
      listen = "127.0.0.1:${toString cfg.port}";
      dataDir = cfg.dataDir;
    }
    // cfg.settings);
in {
  options.services.eve = {
    enable = lib.mkEnableOption "the eve build server";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "The eve package.";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 4790;
      description = "Loopback port eve listens on.";
    };
    dataDir = lib.mkOption {
      type = lib.types.str;
      default = "${config.home.homeDirectory}/.local/share/eve";
      description = "Jobs, uploads, DerivedData, tokens and the database.";
    };
    settings = lib.mkOption {
      type = yaml.type;
      default = {};
      example = lib.literalExpression ''
        {
          team = "2559B88H6C";
          asc = { keyId = "J9584QW2TF"; issuerId = "25a2d76f-…"; keyPath = "~/.appstoreconnect/private_keys/AuthKey_J9584QW2TF.p8"; };
          apps.driver = { bundleId = "ai.selfdeprecated.driver"; testflight.group = "Driver"; };
        }
      '';
      description = "The rest of server.yml: team, asc, apps, sizeGuard, device, notify.";
    };
    claudeSkill = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Install the eve agent skill for Claude Code (~/.claude/skills/eve).";
    };
    tailscaleService = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Whether the host should publish eve as a Tailscale Service. Enable only after the
          Service exists in the Tailscale admin console with this host approved: the host's
          Serve tooling fails until then.
        '';
      };
      name = lib.mkOption {
        type = lib.types.str;
        default = "svc:eve";
        description = "The Tailscale Service name.";
      };
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions = [
        {
          assertion = pkgs.stdenv.hostPlatform.isDarwin;
          message = "services.eve runs on macOS only (it signs with Xcode).";
        }
      ];
      home.packages = [cfg.package];
      # `eve token`, `eve run` and `eve doctor` read server.yml from here by default.
      xdg.configFile."eve/server.yml".source = configFile;

      launchd.agents.eve = {
        enable = true;
        config = {
          ProgramArguments = ["${lib.getExe cfg.package}" "serve" "--config" "${configFile}"];
          LimitLoadToSessionType = "Aqua";
          ProcessType = "Interactive";
          RunAtLoad = true;
          KeepAlive = true;
          ThrottleInterval = 10;
          # Eve builds each child's environment itself; the agent gets Apple's tools only.
          EnvironmentVariables = {
            PATH = "/usr/bin:/bin:/usr/sbin:/sbin";
            LANG = "en_US.UTF-8";
          };
          WorkingDirectory = config.home.homeDirectory;
          StandardOutPath = "${config.home.homeDirectory}/Library/Logs/eve.log";
          StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/eve.log";
        };
      };
    }
    (lib.mkIf cfg.claudeSkill {
      home.file.".claude/skills/eve".source = "${cfg.package}/share/skill/eve";
    })
  ]);
}
