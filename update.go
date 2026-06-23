package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updateRepo is the GitHub "owner/name" the updater pulls releases from. It can
// be overridden with HAMMER_REPO so forks can self-update too.
const defaultUpdateRepo = "chenchaoyi/hammer"

// apiBaseURL and downloadBaseURL are the GitHub endpoints the updater talks to.
// They are package vars (not consts) only so tests can redirect them at a local
// httptest server; production always uses the real GitHub hosts.
var (
	apiBaseURL      = "https://api.github.com"
	downloadBaseURL = "https://github.com"
)

// mirrorProxies mirrors install.sh: when GitHub is unreachable (common in
// mainland China), each proxy is prepended to the GitHub URL as a fallback.
var mirrorProxies = []string{
	"https://ghfast.top/",
	"https://gh-proxy.com/",
	"https://ghproxy.net/",
}

// updateOptions captures the parsed `hammer update` flags.
type updateOptions struct {
	check      bool   // only report whether an update exists; don't install
	target     string // pin a specific release tag (e.g. v1.2.0); empty means latest
	yes        bool   // skip the confirmation prompt
	mirrorMode string // auto | github | ghproxy | <http(s)://proxy/>
}

func updateUsage(fs *flag.FlagSet) func() {
	return func() {
		out := fs.Output()
		fmt.Fprintf(out, `hammer update — replace this binary with the latest release

Usage:
  hammer update [options]

Options:
`)
		fs.PrintDefaults()
		fmt.Fprintf(out, `
Environment:
  HAMMER_REPO              GitHub "owner/name" to update from (default %s)
  HAMMER_INSTALL_MIRROR    auto | github | ghproxy | <http(s)://proxy/> (same as -mirror)

Examples:
  hammer update                 # update to the latest release
  hammer update -check          # report whether a newer version exists
  hammer update -version v1.2.0 # install a specific release
  hammer update -mirror ghproxy # force downloads through the mirror chain
`, defaultUpdateRepo)
	}
}

// runUpdate handles the `update` subcommand. It returns a process exit code.
func runUpdate(args []string) int {
	var opt updateOptions
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.Usage = updateUsage(fs)
	fs.BoolVar(&opt.check, "check", false, "only check for a newer version; do not install")
	fs.StringVar(&opt.target, "version", "", "install this specific release tag (default: latest)")
	fs.BoolVar(&opt.yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&opt.yes, "y", false, "skip the confirmation prompt (shorthand)")
	fs.StringVar(&opt.mirrorMode, "mirror", os.Getenv("HAMMER_INSTALL_MIRROR"),
		"download mirror: auto | github | ghproxy | <http(s)://proxy/>")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		// flag already printed the error and usage.
		return exitUsage
	}
	if opt.mirrorMode == "" {
		opt.mirrorMode = "auto"
	}

	colorErr = colorEnabled(os.Stderr)
	colorOut = colorEnabled(os.Stdout)

	repo := os.Getenv("HAMMER_REPO")
	if repo == "" {
		repo = defaultUpdateRepo
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}

	// --- Resolve the target release ------------------------------------
	var release *ghRelease
	var err error
	if opt.target != "" {
		release, err = fetchReleaseByTag(ctx, client, repo, opt.target)
	} else {
		release, err = fetchLatestRelease(ctx, client, repo)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: fetch release info: %v\n", err)
		return exitSetup
	}

	latest := release.TagName
	current := version

	fmt.Fprintf(os.Stderr, "%s %s\n", po("Current version:", cDim), po(current, cFg, cBold))
	fmt.Fprintf(os.Stderr, "%s  %s\n", po("Latest version:", cDim), po(latest, cFg, cBold))

	if opt.target == "" && !isNewer(current, latest) {
		fmt.Fprintln(os.Stderr, po("hammer is already up to date.", cGreen, cBold))
		return exitOK
	}

	if opt.check {
		fmt.Fprintf(os.Stderr, "%s %s %s\n",
			po("Update available:", cYelHi, cBold), po(current, cDim), po("-> "+latest, cGreen, cBold))
		return exitOK
	}

	// --- Confirm -------------------------------------------------------
	if !opt.yes {
		fmt.Fprintf(os.Stderr, "Update hammer %s -> %s? [y/N] ", current, latest)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Fprintln(os.Stderr, "Update canceled.")
			return exitOK
		}
	}

	// --- Locate this executable ----------------------------------------
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: locate current binary: %v\n", err)
		return exitSetup
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	// --- Download the archive + checksums ------------------------------
	assetName, isZip := assetNameFor(runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/%s/releases/download/%s", downloadBaseURL, repo, latest)

	fmt.Fprintf(os.Stderr, "Downloading %s\n", po(assetName, cCyan))
	archive, err := downloadWithFallback(ctx, client, base+"/"+assetName, opt.mirrorMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: download %s: %v\n", assetName, err)
		return exitSetup
	}

	if os.Getenv("HAMMER_SKIP_CHECKSUM") != "1" {
		sums, serr := downloadWithFallback(ctx, client, base+"/SHA256SUMS", opt.mirrorMode)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "Error: download SHA256SUMS: %v\n", serr)
			return exitSetup
		}
		if verr := verifyChecksum(archive, assetName, sums); verr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", verr)
			return exitSetup
		}
		fmt.Fprintln(os.Stderr, po("Checksum verified.", cDim))
	}

	// --- Extract the new binary ----------------------------------------
	newBin, err := extractBinary(archive, isZip)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: extract binary: %v\n", err)
		return exitSetup
	}

	// --- Atomically replace the running binary -------------------------
	if err := replaceExecutable(exe, newBin); err != nil {
		fmt.Fprintf(os.Stderr, "Error: install new binary: %v\n", err)
		if os.IsPermission(err) {
			fmt.Fprintf(os.Stderr, "Hint: %s is not writable; re-run with elevated privileges (e.g. sudo).\n", exe)
		}
		return exitSetup
	}

	fmt.Fprintf(os.Stderr, "%s hammer updated to %s (%s)\n",
		po("✓", cGreen, cBold), po(latest, cGreen, cBold), exe)
	return exitOK
}

// --- GitHub release lookup ------------------------------------------------

type ghRelease struct {
	TagName string `json:"tag_name"`
}

func fetchLatestRelease(ctx context.Context, client *http.Client, repo string) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBaseURL, repo)
	return getRelease(ctx, client, url)
}

func fetchReleaseByTag(ctx context.Context, client *http.Client, repo, tag string) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", apiBaseURL, repo, tag)
	return getRelease(ctx, client, url)
}

func getRelease(ctx context.Context, client *http.Client, url string) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hammer-updater")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release not found (%s)", url)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("GitHub API returned %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.NewDecoder(res.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release JSON: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release has no tag_name")
	}
	return &rel, nil
}

// --- Download with mirror fallback ---------------------------------------

// candidateURLs builds the ordered list of URLs to try for a GitHub asset,
// honoring the mirror mode the same way install.sh does.
func candidateURLs(githubURL, mirrorMode string) []string {
	switch {
	case mirrorMode == "github":
		return []string{githubURL}
	case strings.HasPrefix(mirrorMode, "http://") || strings.HasPrefix(mirrorMode, "https://"):
		base := strings.TrimRight(mirrorMode, "/") + "/"
		return []string{githubURL, base + githubURL}
	case mirrorMode == "ghproxy":
		out := make([]string, 0, len(mirrorProxies))
		for _, p := range mirrorProxies {
			out = append(out, p+githubURL)
		}
		return out
	default: // "auto"
		out := make([]string, 0, len(mirrorProxies)+1)
		out = append(out, githubURL)
		for _, p := range mirrorProxies {
			out = append(out, p+githubURL)
		}
		return out
	}
}

// downloadWithFallback fetches a URL into a temp file, trying GitHub and any
// configured mirrors in order. It returns the path to the downloaded file.
func downloadWithFallback(ctx context.Context, client *http.Client, githubURL, mirrorMode string) (string, error) {
	var lastErr error
	for i, candidate := range candidateURLs(githubURL, mirrorMode) {
		if i > 0 {
			fmt.Fprintf(os.Stderr, "Retrying via mirror: %s\n", po(mirrorHost(candidate), cDim))
		}
		path, err := downloadTo(ctx, client, candidate)
		if err == nil {
			return path, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no download candidates")
	}
	return "", fmt.Errorf("GitHub and every configured mirror failed: %w", lastErr)
}

func downloadTo(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hammer-updater")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s -> %s", url, res.Status)
	}
	f, err := os.CreateTemp("", "hammer-update-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, res.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if closeErr != nil {
		os.Remove(f.Name())
		return "", closeErr
	}
	return f.Name(), nil
}

// mirrorHost extracts the host portion of a proxied URL for log messages.
func mirrorHost(url string) string {
	s := strings.TrimPrefix(url, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- Checksum verification -----------------------------------------------

func verifyChecksum(archivePath, assetName, sumsPath string) error {
	sums, err := os.Open(sumsPath)
	if err != nil {
		return err
	}
	defer sums.Close()

	var expected string
	scanner := bufio.NewScanner(sums)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == assetName {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if expected == "" {
		return fmt.Errorf("%s not found in SHA256SUMS", assetName)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s:\n  expected: %s\n  actual:   %s", assetName, expected, actual)
	}
	return nil
}

// --- Archive extraction --------------------------------------------------

// assetNameFor returns the release asset filename for an OS/arch pair, and
// whether it is a zip (Windows) rather than a tar.gz.
func assetNameFor(goos, goarch string) (name string, isZip bool) {
	if goos == "windows" {
		return fmt.Sprintf("hammer-%s-%s.zip", goos, goarch), true
	}
	return fmt.Sprintf("hammer-%s-%s.tar.gz", goos, goarch), false
}

// extractBinary pulls the hammer executable out of the downloaded archive and
// writes it to a temp file, returning that path. The caller is responsible for
// moving it into place.
func extractBinary(archivePath string, isZip bool) (string, error) {
	if isZip {
		return extractFromZip(archivePath)
	}
	return extractFromTarGz(archivePath)
}

func extractFromTarGz(archivePath string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if isHammerBinary(filepath.Base(hdr.Name)) {
			return writeTempBinary(tr)
		}
	}
	return "", fmt.Errorf("archive did not contain a hammer binary")
}

func extractFromZip(archivePath string) (string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if isHammerBinary(filepath.Base(file.Name)) {
			rc, err := file.Open()
			if err != nil {
				return "", err
			}
			path, werr := writeTempBinary(rc)
			rc.Close()
			return path, werr
		}
	}
	return "", fmt.Errorf("archive did not contain a hammer binary")
}

func isHammerBinary(name string) bool {
	return name == "hammer" || name == "hammer.exe" || strings.HasPrefix(name, "hammer-")
}

func writeTempBinary(r io.Reader) (string, error) {
	out, err := os.CreateTemp("", "hammer-bin-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, r)
	closeErr := out.Close()
	if err != nil {
		os.Remove(out.Name())
		return "", err
	}
	if closeErr != nil {
		os.Remove(out.Name())
		return "", closeErr
	}
	if err := os.Chmod(out.Name(), 0o755); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// --- Atomic binary replacement -------------------------------------------

// replaceExecutable installs newBin at exe. It writes the new binary next to
// the target (so the final rename is atomic and stays on one filesystem), then
// swaps it in. On Windows the running binary can't be overwritten directly, so
// the old one is moved aside first.
func replaceExecutable(exe, newBin string) error {
	dir := filepath.Dir(exe)

	// Move the new binary alongside the target so rename(2) is atomic.
	staged := filepath.Join(dir, ".hammer-update-"+strconv.Itoa(os.Getpid()))
	if err := moveFile(newBin, staged); err != nil {
		return err
	}
	defer os.Remove(staged) // no-op once the rename below succeeds

	if runtime.GOOS == "windows" {
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(staged, exe); err != nil {
			// Roll back so the user still has a working binary.
			_ = os.Rename(old, exe)
			return err
		}
		os.Remove(old) // best effort; may be locked while running
		return nil
	}

	return os.Rename(staged, exe)
}

// moveFile renames src to dst, falling back to a copy when they live on
// different filesystems (os.CreateTemp uses $TMPDIR, often a separate mount).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	in.Close()
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return closeErr
	}
	os.Remove(src)
	return nil
}

// --- Version comparison ---------------------------------------------------

// isNewer reports whether the candidate release tag is newer than current.
// A "dev" or unparseable current version is always treated as outdated so the
// updater installs a real release over a locally-built binary.
func isNewer(current, candidate string) bool {
	if current == candidate {
		return false
	}
	cur, okCur := parseSemver(current)
	cand, okCand := parseSemver(candidate)
	if !okCur {
		// dev / unknown build: any real release counts as an upgrade.
		return okCand
	}
	if !okCand {
		return false
	}
	for i := 0; i < 3; i++ {
		if cand[i] != cur[i] {
			return cand[i] > cur[i]
		}
	}
	return false
}

// parseSemver parses "vX.Y.Z" (the leading v and any -suffix are optional)
// into [3]int. It returns ok=false for non-semver strings like "dev".
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return out, false
	}
	// Drop any pre-release / build suffix.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
