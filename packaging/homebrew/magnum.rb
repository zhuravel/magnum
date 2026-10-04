# Homebrew formula template: scripts/release.sh fills in the tag and the commit and pushes the result
# to github.com/zhuravel/homebrew-tap as Formula/magnum.rb (`brew install zhuravel/tap/magnum`).
# Homebrew builds it from source, so the binary needs no signing; the git URL also works while the
# repository is private (git uses your credentials).
class Magnum < Formula
  desc "Review GitHub pull requests with AI agents in herdr panes"
  homepage "https://github.com/zhuravel/magnum"
  url "https://github.com/zhuravel/magnum.git", tag: "@VERSION@", revision: "@REVISION@"
  license "MIT"
  head "https://github.com/zhuravel/magnum.git", branch: "master"

  depends_on "go" => :build
  depends_on "gh"
  depends_on :macos

  def install
    system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}"), "./cmd/magnum"
  end

  def caveats
    <<~EOS
      Set up:   magnum init, then magnum install --plugin (launchd agent and herdr plugin)
      Upgrade:  after `brew upgrade magnum`, restart the daemon on the new binary:
                  magnum daemon-restart --drain
      Files:    config and App keys in ~/.config/magnum, the registry in ~/.local/share/magnum,
                logs in ~/.local/state/magnum (`magnum config` prints them)
    EOS
  end

  test do
    assert_match "magnum #{version}", shell_output("#{bin}/magnum version")
  end
end
