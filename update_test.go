package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v1.2", [3]int{1, 2, 0}, true},
		{"v2", [3]int{2, 0, 0}, true},
		{"v1.2.3-rc1", [3]int{1, 2, 3}, true},
		{"v1.2.3+build5", [3]int{1, 2, 3}, true},
		{"dev", [3]int{}, false},
		{"", [3]int{}, false},
		{"v1.2.3.4", [3]int{}, false},
		{"vx.y.z", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSemver(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v2.0.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.1.9", false},
		{"v2.0.0", "v1.9.9", false},
		{"dev", "v1.0.0", true}, // dev always upgrades to a real release
		{"dev", "dev", false},   // identical strings never "newer"
		{"v1.0.0", "garbage", false},
	}
	for _, c := range cases {
		if got := isNewer(c.current, c.candidate); got != c.want {
			t.Errorf("isNewer(%q,%q) = %v; want %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetNameFor(t *testing.T) {
	if name, zipped := assetNameFor("darwin", "arm64"); name != "hammer-darwin-arm64.tar.gz" || zipped {
		t.Errorf("darwin/arm64 = %q,%v", name, zipped)
	}
	if name, zipped := assetNameFor("linux", "amd64"); name != "hammer-linux-amd64.tar.gz" || zipped {
		t.Errorf("linux/amd64 = %q,%v", name, zipped)
	}
	if name, zipped := assetNameFor("windows", "amd64"); name != "hammer-windows-amd64.zip" || !zipped {
		t.Errorf("windows/amd64 = %q,%v", name, zipped)
	}
}

func TestCandidateURLs(t *testing.T) {
	const gh = "https://github.com/o/r/releases/download/v1/hammer-linux-amd64.tar.gz"

	if got := candidateURLs(gh, "github"); len(got) != 1 || got[0] != gh {
		t.Errorf("github mode = %v", got)
	}

	auto := candidateURLs(gh, "auto")
	if len(auto) != len(mirrorProxies)+1 || auto[0] != gh {
		t.Fatalf("auto mode = %v", auto)
	}
	if auto[1] != mirrorProxies[0]+gh {
		t.Errorf("auto mode first mirror = %q", auto[1])
	}

	ghproxy := candidateURLs(gh, "ghproxy")
	if len(ghproxy) != len(mirrorProxies) || ghproxy[0] != mirrorProxies[0]+gh {
		t.Errorf("ghproxy mode = %v", ghproxy)
	}

	custom := candidateURLs(gh, "https://mirror.example.com/")
	if len(custom) != 2 || custom[0] != gh || custom[1] != "https://mirror.example.com/"+gh {
		t.Errorf("custom mode = %v", custom)
	}
	// Trailing slash should be normalized, not doubled.
	custom2 := candidateURLs(gh, "https://mirror.example.com")
	if custom2[1] != "https://mirror.example.com/"+gh {
		t.Errorf("custom mode (no slash) = %q", custom2[1])
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "hammer-linux-amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	hexSum := hex.EncodeToString(sum[:])

	sums := filepath.Join(dir, "SHA256SUMS")
	body := hexSum + "  hammer-linux-amd64.tar.gz\n" +
		"0000000000000000000000000000000000000000000000000000000000000000  other.tar.gz\n"
	if err := os.WriteFile(sums, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(archive, "hammer-linux-amd64.tar.gz", sums); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := verifyChecksum(archive, "missing.tar.gz", sums); err == nil {
		t.Error("expected error for asset missing from SHA256SUMS")
	}

	// Corrupt the archive: checksum must now mismatch.
	if err := os.WriteFile(archive, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, "hammer-linux-amd64.tar.gz", sums); err == nil {
		t.Error("expected checksum mismatch for tampered archive")
	}
}

func TestExtractBinaryTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "hammer.tar.gz")
	writeTarGz(t, archive, "hammer", []byte("#!binary\n"))

	out, err := extractBinary(archive, false)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	defer os.Remove(out)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!binary\n" {
		t.Errorf("extracted content = %q", got)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
}

func TestExtractBinaryTarGzMissing(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "empty.tar.gz")
	writeTarGz(t, archive, "README.md", []byte("not a binary"))
	if _, err := extractBinary(archive, false); err == nil {
		t.Error("expected error when archive has no hammer binary")
	}
}

func TestExtractBinaryZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "hammer.zip")
	writeZip(t, archive, "hammer.exe", []byte("MZbinary"))

	out, err := extractBinary(archive, true)
	if err != nil {
		t.Fatalf("extractBinary zip: %v", err)
	}
	defer os.Remove(out)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "MZbinary" {
		t.Errorf("extracted content = %q", got)
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "hammer")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stage the replacement in a separate temp dir to exercise the
	// cross-filesystem copy fallback path in moveFile.
	newBin := filepath.Join(t.TempDir(), "newbin")
	if err := os.WriteFile(newBin, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(exe, newBin); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("after replace, exe = %q; want %q", got, "new")
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o111 == 0 {
		t.Error("replaced binary lost its executable bit")
	}
}

func TestMirrorHost(t *testing.T) {
	cases := map[string]string{
		"https://ghfast.top/https://github.com/x": "ghfast.top",
		"http://gh-proxy.com/path":                "gh-proxy.com",
		"ghproxy.net":                             "ghproxy.net",
	}
	for in, want := range cases {
		if got := mirrorHost(in); got != want {
			t.Errorf("mirrorHost(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestIsHammerBinary(t *testing.T) {
	for _, name := range []string{"hammer", "hammer.exe", "hammer-linux-amd64"} {
		if !isHammerBinary(name) {
			t.Errorf("isHammerBinary(%q) = false; want true", name)
		}
	}
	for _, name := range []string{"README.md", "install.sh", "SHA256SUMS"} {
		if isHammerBinary(name) {
			t.Errorf("isHammerBinary(%q) = true; want false", name)
		}
	}
}

// --- test helpers ---------------------------------------------------------

func writeTarGz(t *testing.T, path, name string, content []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path, name string, content []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
}
