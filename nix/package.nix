{
  lib,
  buildGoModule,
  # The flake's revision, so `eve version` tells two builds of one version apart.
  rev ? "unknown",
}:
buildGoModule rec {
  pname = "eve";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./..;
    fileset = lib.fileset.unions [../go.mod ../go.sum ../cmd ../internal ../skill];
  };
  # Update after changing go.mod: set to lib.fakeHash, build, copy the hash from the error.
  vendorHash = "sha256-B5CfrhjjRvwJ+e231kijtIfrUbibZGrokUhC8as3XY0=";
  subPackages = ["cmd/eve"];
  env.CGO_ENABLED = 0;
  ldflags = ["-s" "-w" "-X main.version=${version}+${rev}"];
  # The suite builds fake app bundles and spawns process groups; run it in CI/devShell.
  doCheck = false;
  postInstall = ''
    mkdir -p $out/share
    cp -r skill $out/share/skill
  '';
  meta = {
    description = "Fire-and-forget iOS builds on your own Mac";
    mainProgram = "eve";
    platforms = lib.platforms.darwin ++ lib.platforms.linux;
  };
}
