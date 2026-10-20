{
  description = "mekiki - RSSの注目記事をAIが選んで要約する「今日のページ」Webアプリ";

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

        # 単体バイナリ（CGO不要。SQLiteも純Go実装）
        mekiki = pkgs.buildGoModule {
          pname = "mekiki";
          version = "2.0.0";
          src = pkgs.lib.cleanSource ./.;
          vendorHash = "sha256-H8YMpswJ+xbFM8Jph/pqa6fJN8eREj8yyYPNevLT5ME=";
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" "-X main.version=2.0.0" ];
          meta.mainProgram = "mekiki";
        };

        # claude -p 経路を使う場合用。PATHにclaude-codeを含めたラッパー
        mekikiWithClaude = pkgs.writeShellApplication {
          name = "mekiki";
          runtimeInputs = [ mekiki pkgs.claude-code ];
          text = ''exec ${mekiki}/bin/mekiki "$@"'';
        };
      in {
        packages.default = mekiki;
        packages.with-claude = mekikiWithClaude;

        # nix flake check でGoのテストを実行（buildGoModuleのcheckPhase）
        checks.default = mekiki;

        devShells.default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.claude-code ];
        };
      });
}
