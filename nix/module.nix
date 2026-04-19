self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cuproxy;

  defaultPkg = self.packages.${pkgs.stdenv.hostPlatform.system}.default;

  logLevels = [
    "panic"
    "fatal"
    "error"
    "warn"
    "info"
    "debug"
    "trace"
    "disabled"
  ];
in
{
  options.services.cuproxy = {
    enable = lib.mkEnableOption "CUProxy IPP banner-page proxy";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPkg;
      defaultText = lib.literalExpression "cuproxy flake package";
      description = "The cuproxy package to use.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = ":631";
      example = "127.0.0.1:6310";
      description = ''
        Address and port to listen on for incoming IPP connections.
        Port 631 is the standard CUPS/IPP port. The service is granted
        `CAP_NET_BIND_SERVICE` so it can bind privileged ports without root.
      '';
    };

    printerTo = lib.mkOption {
      type = lib.types.str;
      example = "printserver.lan:631/printers/MyPrinter";
      description = ''
        IPP URL of the real printer to forward jobs to, in the form
        `host:port/printers/PrinterName`. This is the only required option.
      '';
    };

    logLevel = lib.mkOption {
      type = lib.types.enum logLevels;
      default = "info";
      description = "Log verbosity level.";
    };

    settings = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = lib.literalExpression ''
        {
          USE_GHOSTSCRIPT      = "true";
          BANNER_APPEND        = "false";
          PDF_PAGE_SIZE        = "A4";
          PDF_LANDSCAPE        = "false";
          PDF_FONT_SIZE        = "12";
          PDF_LEFT_MARGIN      = "10";
          PDF_TOP_MARGIN       = "10";

          WEBHOOKS_TO_CALL     = "auth;POST;https://auth.example.com/banner&&info;GET;https://info.example.com";
          WEBHOOK_MAX_DURATION = "30s";

          CUPSFILTER_LOCATION  = "''${pkgs.cups}/sbin/cupsfilter";
          PPD_LOCATION         = "''${pkgs.cups}/share/ppd/cupsfilters/Generic-PDF_Printer-PDF.ppd";

          DUMP_IPP_CONTENTS    = "/var/log/cuproxy/ipp";
        }
      '';
      description = ''
        Additional environment variables passed to the service. Keys are
        environment variable names, values are their values. See the project
        README for the full list of supported variables.
      '';
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Path to a file containing additional environment variables, one per
        line in `KEY=VALUE` format. Useful for secrets such as
        `WEBHOOK_REQUEST_NONCE` that should not appear in the Nix store.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.cuproxy = {
      isSystemUser = true;
      group = "cuproxy";
      description = "CUProxy service user";
    };
    users.groups.cuproxy = { };

    systemd.services.cuproxy = {
      description = "CUProxy IPP banner-page proxy";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      environment = {
        LISTEN = cfg.listen;
        PRINTER_TO = cfg.printerTo;
        LOG_LEVEL = cfg.logLevel;
        CUPSFILTER_LOCATION = "${pkgs.cups}/sbin/cupsfilter";
        PPD_LOCATION = "${pkgs.cups-filters}/share/ppd/cupsfilters/Generic-PDF_Printer-PDF.ppd";
        HOME = "/var/lib/cuproxy";
        XDG_CONFIG_HOME = "/var/lib/cuproxy/.config";
      }
      // cfg.settings;

      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        User = "cuproxy";
        Group = "cuproxy";

        AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
        CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];

        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ReadWritePaths = [
          "/tmp"
          "/var/lib/cuproxy"
        ];

        RuntimeDirectory = "cuproxy";
        StateDirectory = "cuproxy";
        LogsDirectory = "cuproxy";

        Restart = "on-failure";
        RestartSec = "5s";
      }
      // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}
