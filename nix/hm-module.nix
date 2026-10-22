# Home Manager module: services.autogit
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.autogit;
  yaml = pkgs.formats.yaml { };
in
{
  options.services.autogit = {
    enable = lib.mkEnableOption "autogit, a daemon that keeps git repositories in sync with their remotes";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "autogit.packages.\${system}.default";
      description = "The autogit package to use.";
    };

    settings = lib.mkOption {
      inherit (yaml) type;
      default = { };
      example = lib.literalExpression ''
        {
          defaults.push_remotes = [ "origin" ];
          repos = [
            { path = "~/notes"; }
            { path = "~/journal"; push_remotes = [ "backup" ]; min_commit_interval = "5m"; }
          ];
        }
      '';
      description = ''
        Contents of ~/.config/autogit/config.yaml. See the README for all
        options.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    xdg.configFile."autogit/config.yaml".source = yaml.generate "autogit.yaml" cfg.settings;

    systemd.user.services.autogit = {
      Unit = {
        Description = "autogit: keep git repositories in sync with their remotes";
        After = [ "network-online.target" ];
        X-Restart-Triggers = [ config.xdg.configFile."autogit/config.yaml".source ];
      };
      Service = {
        ExecStart = "${lib.getExe cfg.package} run";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        RestartSec = 10;
      };
      Install.WantedBy = [ "default.target" ];
    };
  };
}
