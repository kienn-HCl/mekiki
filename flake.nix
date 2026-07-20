{
  description = "mekiki - RSSをClaudeで目利きしてDiscordに投稿するbot";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true; # claude-code
        };

        python = pkgs.python3.withPackages (p: [ p.feedparser ]);
        pythonDev = pkgs.python3.withPackages (p: [ p.feedparser p.pytest ]);

        mekiki = pkgs.writeShellApplication {
          name = "mekiki";
          runtimeInputs = [ python pkgs.claude-code ];
          text = ''
            # MEKIKI_DIRが設定されていればそこへcd（cron等、任意のcwdからの起動用）。
            # プロファイル・OPML・キャッシュの相対パスはcwd基準で解決される
            if [ -n "''${MEKIKI_DIR:-}" ]; then
              cd "$MEKIKI_DIR"
            fi
            export PYTHONPATH=${self}''${PYTHONPATH:+:$PYTHONPATH}
            exec python3 -m mekiki "$@"
          '';
        };
      in {
        packages.default = mekiki;

        checks.pytest = pkgs.runCommand "mekiki-pytest" {
          nativeBuildInputs = [ pythonDev ];
        } ''
          cd ${self}
          export PYTHONDONTWRITEBYTECODE=1
          python3 -m pytest tests/ -q -p no:cacheprovider
          touch $out
        '';

        devShells.default = pkgs.mkShell {
          packages = [ pythonDev pkgs.claude-code ];
        };
      });
}
