package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallScriptInstallsReleaseBinaryAsHammer(t *testing.T) {
	releaseDir := t.TempDir()
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}

	asset := "hammer-linux-amd64.tar.gz"
	archivePath := filepath.Join(releaseDir, asset)
	if err := writeFakeHammerArchive(archivePath); err != nil {
		t.Fatalf("write fake release archive: %v", err)
	}
	sum, err := sha256File(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "SHA256SUMS"), []byte(fmt.Sprintf("%s  %s\n", sum, asset)), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "./install.sh")
	cmd.Env = append(os.Environ(),
		"HAMMER_DOWNLOAD_BASE=file://"+releaseDir,
		"HAMMER_INSTALL_DIR="+installDir,
		"HAMMER_OS=linux",
		"HAMMER_ARCH=amd64",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	installed := filepath.Join(installDir, "hammer")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installed hammer missing: %v\noutput:\n%s", err, out)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("installed hammer is not executable: mode %v", info.Mode())
	}
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "fake hammer") {
		t.Fatalf("installed unexpected binary contents:\n%s", data)
	}
}

func TestInstallScriptFallsBackWhenGitHubDownloadFails(t *testing.T) {
	releaseDir, installDir, curlDir, logPath := setupFakeReleaseWithCurl(t)

	cmd := exec.Command("sh", "./install.sh")
	cmd.Env = append(os.Environ(),
		"HAMMER_REPO=example/hammer",
		"HAMMER_VERSION=v9.9.9",
		"HAMMER_INSTALL_DIR="+installDir,
		"HAMMER_OS=linux",
		"HAMMER_ARCH=amd64",
		"HAMMER_FAKE_RELEASE_DIR="+releaseDir,
		"PATH="+curlDir+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	logged := readFile(t, logPath)
	if !strings.Contains(logged, "https://github.com/example/hammer/releases/download/v9.9.9/hammer-linux-amd64.tar.gz") {
		t.Fatalf("fake curl log missing direct GitHub attempt:\n%s", logged)
	}
	if !strings.Contains(logged, "https://ghfast.top/https://github.com/example/hammer/releases/download/v9.9.9/hammer-linux-amd64.tar.gz") {
		t.Fatalf("fake curl log missing mirror fallback attempt:\n%s", logged)
	}
	if !strings.Contains(string(out), "retrying via ghfast.top mirror") {
		t.Fatalf("installer did not explain mirror fallback:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "hammer")); err != nil {
		t.Fatalf("hammer was not installed after fallback: %v\n%s", err, out)
	}
}

func TestInstallScriptGithubMirrorModeDoesNotTryFallback(t *testing.T) {
	releaseDir, installDir, curlDir, logPath := setupFakeReleaseWithCurl(t)

	cmd := exec.Command("sh", "./install.sh")
	cmd.Env = append(os.Environ(),
		"HAMMER_REPO=example/hammer",
		"HAMMER_VERSION=v9.9.9",
		"HAMMER_INSTALL_DIR="+installDir,
		"HAMMER_OS=linux",
		"HAMMER_ARCH=amd64",
		"HAMMER_INSTALL_MIRROR=github",
		"HAMMER_FAKE_RELEASE_DIR="+releaseDir,
		"PATH="+curlDir+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh unexpectedly succeeded:\n%s", out)
	}

	logged := readFile(t, logPath)
	if strings.Contains(logged, "ghfast.top") {
		t.Fatalf("github-only mode tried mirror fallback:\n%s", logged)
	}
	if !strings.Contains(string(out), "GitHub and every configured mirror failed") {
		t.Fatalf("failure did not explain exhausted downloads:\n%s", out)
	}
}

func TestInstallScriptGhproxyModeSkipsDirectGitHub(t *testing.T) {
	releaseDir, installDir, curlDir, logPath := setupFakeReleaseWithCurl(t)

	cmd := exec.Command("sh", "./install.sh")
	cmd.Env = append(os.Environ(),
		"HAMMER_REPO=example/hammer",
		"HAMMER_VERSION=v9.9.9",
		"HAMMER_INSTALL_DIR="+installDir,
		"HAMMER_OS=linux",
		"HAMMER_ARCH=amd64",
		"HAMMER_INSTALL_MIRROR=ghproxy",
		"HAMMER_FAKE_RELEASE_DIR="+releaseDir,
		"PATH="+curlDir+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	logged := readFile(t, logPath)
	if strings.Contains(logged, "\nhttps://github.com/") || strings.HasPrefix(logged, "https://github.com/") {
		t.Fatalf("ghproxy mode tried direct GitHub:\n%s", logged)
	}
	if !strings.Contains(logged, "https://ghfast.top/https://github.com/example/hammer/releases/download/v9.9.9/hammer-linux-amd64.tar.gz") {
		t.Fatalf("ghproxy mode did not try mirror URL:\n%s", logged)
	}
}

func TestInstallDocsUseReleaseCurlOneLiner(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "curl -fsSL https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh | sh"
	if !strings.Contains(string(data), want) {
		t.Fatalf("README.md missing release installer one-liner %q", want)
	}
	if !strings.Contains(string(data), "HAMMER_INSTALL_MIRROR=ghproxy") {
		t.Fatal("README.md missing mirror fallback install guidance")
	}
	if !strings.Contains(string(data), "https://ghfast.top/https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh") {
		t.Fatal("README.md missing bootstrap mirror guidance for install.sh itself")
	}
}

// Releases are cut by GoReleaser; the curl one-liner depends on install.sh
// being attached to every release. GoReleaser does that via release.extra_files
// in .goreleaser.yaml, so assert the installer is wired in there.
func TestReleaseWorkflowPublishesInstaller(t *testing.T) {
	data, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "./install.sh") {
		t.Fatal(".goreleaser.yaml does not publish install.sh (release.extra_files)")
	}
	// The archive/checksum names must stay stable so install.sh and
	// `hammer update` keep resolving them.
	for _, want := range []string{
		`name_template: "hammer-{{ .Os }}-{{ .Arch }}"`,
		`name_template: "SHA256SUMS"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf(".goreleaser.yaml missing stable asset name %q", want)
		}
	}

	// And the workflow must actually invoke GoReleaser.
	wf, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wf), "goreleaser/goreleaser-action") {
		t.Fatal("release workflow no longer runs goreleaser-action")
	}
}

func writeFakeHammerArchive(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	body := []byte("#!/bin/sh\necho fake hammer\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "hammer",
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		return err
	}
	_, err = tw.Write(body)
	return err
}

func setupFakeReleaseWithCurl(t *testing.T) (releaseDir, installDir, curlDir, logPath string) {
	t.Helper()
	releaseDir = t.TempDir()
	installDir = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}

	asset := "hammer-linux-amd64.tar.gz"
	archivePath := filepath.Join(releaseDir, asset)
	if err := writeFakeHammerArchive(archivePath); err != nil {
		t.Fatalf("write fake release archive: %v", err)
	}
	sum, err := sha256File(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "SHA256SUMS"), []byte(fmt.Sprintf("%s  %s\n", sum, asset)), 0o644); err != nil {
		t.Fatal(err)
	}

	curlDir = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "curl.log")
	fakeCurl := filepath.Join(curlDir, "curl")
	script := `#!/bin/sh
set -eu
out=""
url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
		-o|--output)
			out="$2"
			shift 2
			;;
		--max-time)
			shift 2
			;;
		-*)
			shift
			;;
		*)
			url="$1"
			shift
			;;
	esac
done
echo "$url" >> "$HAMMER_FAKE_CURL_LOG"
case "$url" in
	https://github.com/*)
		exit 7
		;;
	https://ghfast.top/https://github.com/*|https://gh-proxy.com/https://github.com/*|https://ghproxy.net/https://github.com/*)
		name="${url##*/}"
		cp "$HAMMER_FAKE_RELEASE_DIR/$name" "$out"
		;;
	*)
		exit 7
		;;
esac
`
	if err := os.WriteFile(fakeCurl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAMMER_FAKE_CURL_LOG", logPath)
	return releaseDir, installDir, curlDir, logPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
