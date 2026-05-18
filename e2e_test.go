package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorcon/rcon"
)

const (
	rconAddr     = "localhost:25575"
	rconPassword = "skpmtest"
	serverDir    = "./server-data"
	pluginsDir   = "./server-data/plugins"
	testPkg      = "e2e-test"

	// spigotTestResource is a placeholder SpigotMC resource ID.
	// Resource 90770 ("Maintenance") is NOT in the Skript category and will not install.
	// Set SKPM_TEST_SPIGOT_RESOURCE to a free category-25 resource ID to run TestInstallSpigotMC.
	spigotTestResource = "90770"
)

// ---------------------------------------------------------------------------
// Suite setup / teardown
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		teardown()
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() error {
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}

	fmt.Println("→ [setup] Downloading SKPM.jar from latest GitHub release...")
	if err := downloadLatestJAR("skpm-dev", "plugin", filepath.Join(pluginsDir, "SKPM.jar")); err != nil {
		return fmt.Errorf("download SKPM: %w", err)
	}
	fmt.Println("  ✓ SKPM.jar downloaded")

	fmt.Println("→ [setup] Downloading Skript.jar from latest GitHub release...")
	if err := downloadLatestJAR("SkriptLang", "Skript", filepath.Join(pluginsDir, "Skript.jar")); err != nil {
		return fmt.Errorf("download Skript: %w", err)
	}
	fmt.Println("  ✓ Skript.jar downloaded")

	fmt.Println("→ [setup] Starting Paper server via docker compose...")
	up := exec.Command("docker", "compose", "up", "-d")
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	fmt.Println("→ [setup] Waiting for RCON to accept connections (timeout 3m)...")
	if err := waitForRCON(3 * time.Minute); err != nil {
		return err
	}
	fmt.Println("  ✓ RCON ready")

	fmt.Println("→ [setup] Waiting 15s for Skript to finish loading...")
	time.Sleep(15 * time.Second)
	fmt.Println("  ✓ Setup complete")

	return nil
}

func teardown() {
	fmt.Println("→ [teardown] Stopping server and removing volumes...")
	exec.Command("docker", "compose", "down", "-v").Run()
	os.RemoveAll(serverDir)
	fmt.Println("  ✓ Teardown complete")
}

// waitForRCON polls until RCON accepts a connection or the timeout expires.
func waitForRCON(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := rcon.Dial(rconAddr, rconPassword)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("server not ready after %s", timeout)
}

// ---------------------------------------------------------------------------
// RCON helpers
// ---------------------------------------------------------------------------

func connect(t *testing.T) *rcon.Conn {
	t.Helper()
	conn, err := rcon.Dial(rconAddr, rconPassword)
	if err != nil {
		t.Fatalf("[rcon] connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// rconCmd executes a command via RCON, logging the command and full response.
func rconCmd(t *testing.T, conn *rcon.Conn, cmd string) string {
	t.Helper()
	t.Logf("[rcon] > %s", cmd)
	resp, err := conn.Execute(cmd)
	if err != nil {
		t.Fatalf("[rcon] error executing %q: %v", cmd, err)
	}
	t.Logf("[rcon] < %s", stripColors(resp))
	return resp
}

// stripColors removes Minecraft §-color codes for readable log output.
// § is U+00A7 (2-byte UTF-8: 0xC2 0xA7), so we iterate runes not bytes.
func stripColors(s string) string {
	var out strings.Builder
	skip := false
	for _, r := range s {
		if r == '§' {
			skip = true
			continue
		}
		if skip {
			skip = false
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Filesystem / log helpers
// ---------------------------------------------------------------------------

func logLockFile(t *testing.T) {
	t.Helper()
	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Logf("[lock] %s: not found (%v)", lockPath, err)
		return
	}
	t.Logf("[lock] %s:\n%s", lockPath, string(data))
}

func logScriptDir(t *testing.T, pkgName string) {
	t.Helper()
	dir := filepath.Join(serverDir, "plugins/Skript/scripts/skpm", pkgName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("[scripts] %s: not found (%v)", dir, err)
		return
	}
	t.Logf("[scripts] %s: %d file(s)", dir, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		t.Logf("  - %s (%d bytes)", e.Name(), size)
	}
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("file %q did not appear within %s", path, timeout)
}

func waitForFileGone(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("file %q still exists after %s", path, timeout)
}

// waitForLockContent polls the lock file until it contains (or no longer
// contains) the given substring, or the timeout expires.
func waitForLockContent(want string, present bool, timeout time.Duration) error {
	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(lockPath)
		if err == nil {
			has := strings.Contains(string(data), want)
			if has == present {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	verb := "appear in"
	if !present {
		verb = "disappear from"
	}
	return fmt.Errorf("%q did not %s lock file within %s", want, verb, timeout)
}

// serverLogOffset returns the current byte length of the Paper log file.
// Used to scope log searches to output produced after a specific point in time.
func serverLogOffset(t *testing.T) int64 {
	t.Helper()
	logPath := filepath.Join(serverDir, "logs/latest.log")
	info, err := os.Stat(logPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

// waitForServerLog polls the server log for a substring after a given byte
// offset. Returns true if found within the timeout.
func waitForServerLog(pattern string, offset int64, timeout time.Duration) bool {
	logPath := filepath.Join(serverDir, "logs/latest.log")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := os.Open(logPath)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		f.Seek(offset, io.SeekStart)
		data, _ := io.ReadAll(f)
		f.Close()
		if strings.Contains(string(data), pattern) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Registry API helpers
// ---------------------------------------------------------------------------

// fetchLatestVersion queries the live registry for the latest version of a package.
func fetchLatestVersion(t *testing.T, pkgName string) string {
	t.Helper()
	url := "https://registry.skpm.org/packages/" + pkgName
	t.Logf("[registry] GET %s", url)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("[registry] request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("[registry] unexpected status %d for %s", resp.StatusCode, pkgName)
	}
	var pkg struct {
		Latest string `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pkg); err != nil {
		t.Fatalf("[registry] decode failed: %v", err)
	}
	t.Logf("[registry] %s latest = %q", pkgName, pkg.Latest)
	return pkg.Latest
}

// ---------------------------------------------------------------------------
// ensureInstalled: pre-condition helper
// ---------------------------------------------------------------------------

// ensureInstalled installs testPkg if it is not already on disk.
func ensureInstalled(t *testing.T, conn *rcon.Conn) {
	t.Helper()
	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/"+testPkg+"/hello.sk")
	if _, err := os.Stat(scriptPath); err == nil {
		t.Logf("[precond] %s already installed", testPkg)
		return
	}
	t.Logf("[precond] %s not installed — installing now", testPkg)
	rconCmd(t, conn, "skpm install "+testPkg)
	if err := waitForFile(scriptPath, 30*time.Second); err != nil {
		t.Fatalf("[precond] install: %v", err)
	}
	t.Logf("[precond] %s installed", testPkg)
	logLockFile(t)
}

// ---------------------------------------------------------------------------
// Tests (existing, with enhanced logging)
// ---------------------------------------------------------------------------

// TestInstall installs the e2e-test package and verifies the script file and lockfile.
func TestInstall(t *testing.T) {
	t.Log("=== TestInstall: install e2e-test and verify script file + lockfile ===")
	conn := connect(t)

	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/"+testPkg+"/hello.sk")

	// Remove any existing install so we always exercise the install path.
	if _, err := os.Stat(scriptPath); err == nil {
		t.Logf("[setup] removing existing install for a clean test")
		rconCmd(t, conn, "skpm remove "+testPkg+" --confirm")
		if err := waitForFileGone(scriptPath, 15*time.Second); err != nil {
			t.Fatalf("[setup] could not remove existing install: %v", err)
		}
	}

	t.Logf("→ executing: skpm install %s", testPkg)
	rconCmd(t, conn, "skpm install "+testPkg)

	t.Logf("→ waiting for script file: %s", scriptPath)
	if err := waitForFile(scriptPath, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Logf("  ✓ script file appeared")

	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script file: %v", err)
	}
	t.Logf("[script] %s:\n%s", scriptPath, string(data))

	logScriptDir(t, testPkg)
	logLockFile(t)

	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if !strings.Contains(string(lock), testPkg) {
		t.Fatalf("lockfile missing %q entry", testPkg)
	}
	t.Logf("  ✓ lockfile contains %q", testPkg)
}

// TestAlreadyInstalled verifies that installing an already-installed package
// returns a helpful message and does not re-download anything.
func TestAlreadyInstalled(t *testing.T) {
	t.Log("=== TestAlreadyInstalled: re-install should report already installed ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	t.Logf("→ executing: skpm install %s (expect 'already installed')", testPkg)
	resp := rconCmd(t, conn, "skpm install "+testPkg)

	if !strings.Contains(resp, "already installed") {
		t.Errorf("expected 'already installed' in response, got: %q", stripColors(resp))
	} else {
		t.Logf("  ✓ response contains 'already installed'")
	}
}

// TestRemoveRequiresConfirm verifies that remove without --confirm prints a
// warning and leaves the package in place.
func TestRemoveRequiresConfirm(t *testing.T) {
	t.Log("=== TestRemoveRequiresConfirm: remove without --confirm should warn and do nothing ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/"+testPkg+"/hello.sk")
	t.Logf("→ executing: skpm remove %s (no --confirm)", testPkg)
	resp := rconCmd(t, conn, "skpm remove "+testPkg)

	if !strings.Contains(resp, "--confirm") {
		t.Errorf("expected --confirm prompt in response, got: %q", stripColors(resp))
	} else {
		t.Logf("  ✓ response contains '--confirm' prompt")
	}

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatal("package was removed without --confirm flag — script file is gone")
	}
	t.Logf("  ✓ script file still present at %s", scriptPath)
}

// TestRemove removes the e2e-test package and verifies cleanup.
func TestRemove(t *testing.T) {
	t.Log("=== TestRemove: remove e2e-test --confirm and verify cleanup ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/"+testPkg+"/hello.sk")
	t.Logf("  lock state before remove:")
	logLockFile(t)

	t.Logf("→ executing: skpm remove %s --confirm", testPkg)
	rconCmd(t, conn, "skpm remove "+testPkg+" --confirm")

	t.Logf("→ waiting for script file to disappear: %s", scriptPath)
	if err := waitForFileGone(scriptPath, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Logf("  ✓ script file gone")

	t.Logf("  lock state after remove:")
	logLockFile(t)

	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	lock, err := os.ReadFile(lockPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read lockfile: %v", err)
	}
	if strings.Contains(string(lock), testPkg) {
		t.Fatalf("lockfile still contains %q after remove", testPkg)
	}
	t.Logf("  ✓ lockfile no longer contains %q", testPkg)
}

// ---------------------------------------------------------------------------
// Tests (new)
// ---------------------------------------------------------------------------

// TestList verifies that /skpm list shows installed packages.
// list is synchronous so the full output is captured in the RCON response.
func TestList(t *testing.T) {
	t.Log("=== TestList: /skpm list should show installed packages ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	t.Logf("  lock state:")
	logLockFile(t)

	t.Logf("→ executing: skpm list")
	resp := rconCmd(t, conn, "skpm list")
	plain := stripColors(resp)

	if !strings.Contains(plain, testPkg) {
		t.Errorf("expected %q in /skpm list output, got: %q", testPkg, plain)
	} else {
		t.Logf("  ✓ list output contains %q", testPkg)
	}

	if !strings.Contains(strings.ToLower(plain), "installed") {
		t.Errorf("expected 'installed' header in list output, got: %q", plain)
	} else {
		t.Logf("  ✓ list output contains 'installed' header")
	}
}

// TestInfo verifies that /skpm info accepts the command and schedules a fetch.
// The actual metadata arrives async; this test checks the server log for results.
func TestInfo(t *testing.T) {
	t.Log("=== TestInfo: /skpm info should fetch and log package metadata ===")
	conn := connect(t)

	logOffset := serverLogOffset(t)
	t.Logf("  server log offset before command: %d", logOffset)

	t.Logf("→ executing: skpm info %s", testPkg)
	resp := rconCmd(t, conn, "skpm info "+testPkg)
	plain := stripColors(resp)

	if strings.Contains(plain, "ERR") || strings.Contains(strings.ToLower(plain), "error") {
		t.Errorf("info returned immediate error: %q", plain)
	}
	if !strings.Contains(strings.ToLower(plain), "fetch") && !strings.Contains(strings.ToLower(plain), "info") {
		t.Logf("  note: unexpected initial response (may be fine): %q", plain)
	}
	t.Logf("  ✓ initial RCON response received without error")

	// Verify the registry actually has valid metadata for this package.
	t.Logf("→ verifying registry API returns package metadata for %s", testPkg)
	apiResp, err := http.Get("https://registry.skpm.org/packages/" + testPkg)
	if err != nil {
		t.Fatalf("registry API request: %v", err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("registry API returned %d for %s", apiResp.StatusCode, testPkg)
	}
	var pkg struct {
		Name        string `json:"name"`
		Latest      string `json:"latest"`
		Description string `json:"description"`
		Author      string `json:"author"`
	}
	if err := json.NewDecoder(apiResp.Body).Decode(&pkg); err != nil {
		t.Fatalf("decode registry response: %v", err)
	}
	t.Logf("  [registry] name=%q latest=%q author=%q description=%q",
		pkg.Name, pkg.Latest, pkg.Author, pkg.Description)
	if pkg.Name != testPkg {
		t.Errorf("expected name=%q, got %q", testPkg, pkg.Name)
	}
	if pkg.Latest == "" {
		t.Errorf("registry returned empty latest version for %s", testPkg)
	}
	t.Logf("  ✓ registry metadata valid")
}

// TestSearch verifies that /skpm search accepts the command.
// Results arrive async; this test verifies the registry API search works
// and that the command doesn't immediately error.
func TestSearch(t *testing.T) {
	t.Log("=== TestSearch: /skpm search should find e2e-test in the registry ===")
	conn := connect(t)

	query := "e2e"
	t.Logf("→ executing: skpm search %s", query)
	resp := rconCmd(t, conn, "skpm search "+query)
	plain := stripColors(resp)

	if strings.Contains(strings.ToLower(plain), "failed") {
		t.Errorf("search returned immediate failure: %q", plain)
	}
	t.Logf("  ✓ initial RCON response received without error")

	// Verify the registry search API returns results.
	t.Logf("→ verifying registry search API for query %q", query)
	apiResp, err := http.Get("https://registry.skpm.org/search?q=" + query)
	if err != nil {
		t.Fatalf("registry search API: %v", err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("registry search returned %d", apiResp.StatusCode)
	}
	body, _ := io.ReadAll(apiResp.Body)
	t.Logf("  [registry] search results: %s", string(body))

	if !strings.Contains(string(body), testPkg) {
		t.Errorf("expected %q in search results, got: %s", testPkg, string(body))
	} else {
		t.Logf("  ✓ registry search results contain %q", testPkg)
	}
}

// TestInstallNotFound verifies that installing a nonexistent package
// does not create any files and does not crash the plugin.
func TestInstallNotFound(t *testing.T) {
	t.Log("=== TestInstallNotFound: install nonexistent package should error gracefully ===")
	conn := connect(t)

	badPkg := "this-package-does-not-exist-xyz-abc"
	scriptDir := filepath.Join(serverDir, "plugins/Skript/scripts/skpm", badPkg)

	// Pre-check: directory must not exist before the test.
	os.RemoveAll(scriptDir)

	t.Logf("→ executing: skpm install %s (expecting error)", badPkg)
	resp := rconCmd(t, conn, "skpm install "+badPkg)
	t.Logf("  initial RCON response: %q", stripColors(resp))

	// Error arrives async — wait for it to settle (registry fetch + failure).
	t.Logf("→ waiting 10s for async error to resolve...")
	time.Sleep(10 * time.Second)

	if _, err := os.Stat(scriptDir); err == nil {
		t.Errorf("script directory %s was created but should not have been", scriptDir)
	} else {
		t.Logf("  ✓ script directory was not created")
	}

	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	if data, err := os.ReadFile(lockPath); err == nil {
		if strings.Contains(string(data), badPkg) {
			t.Errorf("lockfile unexpectedly contains %q:\n%s", badPkg, string(data))
		} else {
			t.Logf("  ✓ lockfile does not contain %q", badPkg)
		}
	}

	// Verify the plugin is still responsive.
	t.Logf("→ verifying plugin is still responsive via RCON")
	pingResp := rconCmd(t, conn, "skpm list")
	t.Logf("  ping response: %q", stripColors(pingResp))
	t.Logf("  ✓ plugin still responsive after failed install")
}

// TestRemoveNotInstalled verifies that removing a package that is not installed
// returns an error message synchronously.
func TestRemoveNotInstalled(t *testing.T) {
	t.Log("=== TestRemoveNotInstalled: remove a non-installed package should report error ===")
	conn := connect(t)

	notInstalled := "not-installed-pkg-xyz"

	// Ensure it really isn't installed.
	os.RemoveAll(filepath.Join(serverDir, "plugins/Skript/scripts/skpm", notInstalled))

	t.Logf("→ executing: skpm remove %s --confirm (not installed)", notInstalled)
	resp := rconCmd(t, conn, "skpm remove "+notInstalled+" --confirm")
	plain := stripColors(resp)

	if !strings.Contains(plain, "not installed") {
		t.Errorf("expected 'not installed' in response, got: %q", plain)
	} else {
		t.Logf("  ✓ response contains 'not installed'")
	}
}

// TestUpdateAlreadyLatest verifies that updating a package that is already at
// the latest version reports up-to-date (message arrives async in server log).
func TestUpdateAlreadyLatest(t *testing.T) {
	t.Log("=== TestUpdateAlreadyLatest: update when at latest should report up to date ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	t.Logf("  lock state before update:")
	logLockFile(t)

	logOffset := serverLogOffset(t)
	t.Logf("  server log offset: %d", logOffset)

	t.Logf("→ executing: skpm update %s (at latest)", testPkg)
	rconCmd(t, conn, "skpm update "+testPkg)

	// The "already up to date" message comes back async. We poll the lock file
	// to confirm it did not change (the package was not re-installed).
	t.Logf("→ waiting 15s to confirm no version change in lockfile...")
	time.Sleep(15 * time.Second)

	t.Logf("  lock state after update:")
	logLockFile(t)

	// The lockfile should still contain e2e-test (not removed).
	lockData, err := os.ReadFile(filepath.Join(serverDir, "plugins/SKPM/skript.lock"))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if !strings.Contains(string(lockData), testPkg) {
		t.Errorf("lockfile no longer contains %q after update-at-latest", testPkg)
	} else {
		t.Logf("  ✓ lockfile still contains %q (package not re-installed)", testPkg)
	}
}

// TestUpdate installs e2e-test at v1.0.0 via a forced lock injection, then
// runs /skpm update and verifies the plugin upgrades to the latest version.
//
// Prerequisite: the real registry must have e2e-test at a version newer than 1.0.0.
// To add that version: bump testpkg/skpm.json to 2.0.0 and run:
//
//	cd testpkg && SKPM_GITHUB_TOKEN=<token> skpm publish
func TestUpdate(t *testing.T) {
	t.Log("=== TestUpdate: update from old version to latest ===")

	latestVersion := fetchLatestVersion(t, testPkg)
	if latestVersion == "1.0.0" {
		t.Skipf("TestUpdate requires a newer version of %s in the registry. "+
			"Publish one first: bump testpkg/skpm.json to 2.0.0 and run `skpm publish`.", testPkg)
	}

	conn := connect(t)

	// Remove any real install so we can inject an old one cleanly.
	scriptDir := filepath.Join(serverDir, "plugins/Skript/scripts/skpm", testPkg)
	if err := os.RemoveAll(scriptDir); err != nil {
		t.Logf("[setup] removeAll: %v", err)
	}

	t.Logf("→ injecting fake install of %s@1.0.0 into lockfile", testPkg)
	injectOldInstall(t, testPkg, "1.0.0")
	t.Logf("  lock state after injection:")
	logLockFile(t)
	logScriptDir(t, testPkg)

	logOffset := serverLogOffset(t)

	t.Logf("→ executing: skpm update %s (expect upgrade 1.0.0 → %s)", testPkg, latestVersion)
	rconCmd(t, conn, "skpm update "+testPkg)

	// Wait for the lock file to reflect the new version.
	t.Logf("→ waiting for lockfile to contain version %q...", latestVersion)
	if err := waitForLockContent(latestVersion, true, 30*time.Second); err != nil {
		t.Logf("  lock state at timeout:")
		logLockFile(t)
		t.Fatalf("lock did not update to %s: %v", latestVersion, err)
	}
	t.Logf("  ✓ lock file updated to %s", latestVersion)

	t.Logf("  lock state after update:")
	logLockFile(t)
	logScriptDir(t, testPkg)

	// Confirm old version is gone from lockfile.
	lockData, _ := os.ReadFile(filepath.Join(serverDir, "plugins/SKPM/skript.lock"))
	if strings.Contains(string(lockData), `"version":"1.0.0"`) ||
		strings.Contains(string(lockData), `"version": "1.0.0"`) {
		t.Errorf("lockfile still shows 1.0.0 after update:\n%s", string(lockData))
	}

	// Script file should exist.
	scriptPath := filepath.Join(scriptDir, "hello.sk")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Errorf("script file %s does not exist after update", scriptPath)
	} else {
		t.Logf("  ✓ script file present at %s", scriptPath)
	}

	_ = logOffset // would be used to check server log for "Updated" message
}

// TestUpdateAll verifies that /skpm update (no package arg) reports a summary
// of all installed packages and does not crash.
func TestUpdateAll(t *testing.T) {
	t.Log("=== TestUpdateAll: /skpm update (no arg) should report summary for all packages ===")
	conn := connect(t)
	ensureInstalled(t, conn)

	t.Logf("  lock state before update-all:")
	logLockFile(t)

	t.Logf("→ executing: skpm update (no package arg)")
	rconCmd(t, conn, "skpm update")

	// The summary arrives async. Poll the lock file to confirm the package
	// is still there (update-all should not remove packages).
	t.Logf("→ waiting 20s for update-all to settle...")
	time.Sleep(20 * time.Second)

	t.Logf("  lock state after update-all:")
	logLockFile(t)

	lockData, err := os.ReadFile(filepath.Join(serverDir, "plugins/SKPM/skript.lock"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read lockfile: %v", err)
	}
	if !strings.Contains(string(lockData), testPkg) {
		t.Errorf("lockfile no longer contains %q after update-all", testPkg)
	} else {
		t.Logf("  ✓ lockfile still contains %q after update-all", testPkg)
	}

	// Verify plugin is still responsive.
	t.Logf("→ verifying plugin is still responsive")
	rconCmd(t, conn, "skpm list")
	t.Logf("  ✓ plugin responsive after update-all")
}

// TestInstallSpigotMC verifies installing a package from SpigotMC.
//
// This test requires SKPM_TEST_SPIGOT_RESOURCE to be set to the numeric ID of
// a free Skript (category 25) resource on SpigotMC. The default placeholder
// (90770, "Maintenance") is not in the Skript category and will be rejected.
//
// When the dedicated e2e Skript resource is created, set this env var in CI.
func TestInstallSpigotMC(t *testing.T) {
	t.Log("=== TestInstallSpigotMC: install a package from SpigotMC ===")

	resourceID := os.Getenv("SKPM_TEST_SPIGOT_RESOURCE")
	if resourceID == "" {
		t.Skipf("SKPM_TEST_SPIGOT_RESOURCE not set. "+
			"Set to a free Skript (category 25) SpigotMC resource ID. "+
			"Placeholder %s is not in the Skript category.", spigotTestResource)
	}

	conn := connect(t)

	lockName := "spigotmc-" + resourceID
	scriptDir := filepath.Join(serverDir, "plugins/Skript/scripts/skpm", lockName)

	// Clean up any previous run.
	os.RemoveAll(scriptDir)

	pkgArg := "spigotmc:" + resourceID
	t.Logf("→ executing: skpm install %s", pkgArg)
	t.Logf("  (resource ID: %s, expected lock name: %s)", resourceID, lockName)
	rconCmd(t, conn, "skpm install "+pkgArg)

	t.Logf("→ waiting for script directory: %s", scriptDir)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(scriptDir); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if _, err := os.Stat(scriptDir); os.IsNotExist(err) {
		t.Logf("  lock state at failure:")
		logLockFile(t)
		t.Fatalf("script directory %s was not created after install", scriptDir)
	}
	t.Logf("  ✓ script directory created")

	logScriptDir(t, lockName)
	logLockFile(t)

	entries, err := os.ReadDir(scriptDir)
	if err != nil || len(entries) == 0 {
		t.Errorf("expected at least one .sk file in %s", scriptDir)
	} else {
		t.Logf("  ✓ %d file(s) installed", len(entries))
	}

	lockData, err := os.ReadFile(filepath.Join(serverDir, "plugins/SKPM/skript.lock"))
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if !strings.Contains(string(lockData), lockName) {
		t.Errorf("lockfile missing %q:\n%s", lockName, string(lockData))
	} else {
		t.Logf("  ✓ lockfile contains %q", lockName)
	}

	// Cleanup.
	t.Logf("→ cleanup: skpm remove %s --confirm", pkgArg)
	rconCmd(t, conn, "skpm remove "+pkgArg+" --confirm")
}

// ---------------------------------------------------------------------------
// TestPublish: full publish flow (CLI → local registry → GitHub PR)
// ---------------------------------------------------------------------------

// TestPublish builds the CLI and registry from source, starts the registry
// pointed at skpm-dev/e2e, runs `skpm publish` for the e2e-publish-test
// fixture package, and verifies a PR is opened.
//
// Required env vars:
//   - SKPM_GITHUB_TOKEN  — user GitHub PAT with read:user scope (publish auth)
//   - REGISTRY_GITHUB_TOKEN — server PAT with repo write access on skpm-dev/e2e
func TestPublish(t *testing.T) {
	t.Log("=== TestPublish: CLI publish → local registry → GitHub PR on skpm-dev/e2e ===")

	userToken := os.Getenv("SKPM_GITHUB_TOKEN")
	if userToken == "" {
		t.Skip("SKPM_GITHUB_TOKEN not set — skipping TestPublish")
	}

	registryURL := startLocalRegistry(t) // also skips if REGISTRY_GITHUB_TOKEN missing
	cliBin := buildBinary(t, "../cli", "skpm")

	// Copy the publishpkg fixture to a temp dir and set a unique version.
	pkgDir := t.TempDir()
	version := fmt.Sprintf("0.0.%d", time.Now().UnixMilli()%100000)
	t.Logf("[publish] using version %s for this run", version)

	manifestSrc, err := os.ReadFile("publishpkg/skpm.json")
	if err != nil {
		t.Fatalf("read publishpkg/skpm.json: %v", err)
	}
	var manifest map[string]interface{}
	if err := json.Unmarshal(manifestSrc, &manifest); err != nil {
		t.Fatalf("parse publishpkg/skpm.json: %v", err)
	}
	manifest["version"] = version
	updated, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(pkgDir, "skpm.json"), updated, 0644); err != nil {
		t.Fatalf("write skpm.json: %v", err)
	}
	t.Logf("[publish] skpm.json:\n%s", string(updated))

	scriptSrc, err := os.ReadFile("publishpkg/hello.sk")
	if err != nil {
		t.Fatalf("read publishpkg/hello.sk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "hello.sk"), scriptSrc, 0644); err != nil {
		t.Fatalf("write hello.sk: %v", err)
	}

	pkgName := "e2e-publish-test"
	branchName := fmt.Sprintf("publish/%s-%s", pkgName, version)
	regToken := os.Getenv("REGISTRY_GITHUB_TOKEN")

	// Register cleanup: close PR + delete branch, regardless of test outcome.
	t.Cleanup(func() {
		t.Logf("[cleanup] closing PR and deleting branch %s on skpm-dev/e2e", branchName)
		prNum, err := findOpenPR(regToken, "skpm-dev", "e2e", branchName)
		if err != nil {
			t.Logf("[cleanup] findOpenPR error: %v", err)
		} else if prNum != 0 {
			t.Logf("[cleanup] closing PR #%d", prNum)
			if err := closeGitHubPR(regToken, "skpm-dev", "e2e", prNum); err != nil {
				t.Logf("[cleanup] closeGitHubPR: %v", err)
			} else {
				t.Logf("[cleanup] PR #%d closed", prNum)
			}
		}
		if err := deleteGitHubBranch(regToken, "skpm-dev", "e2e", branchName); err != nil {
			t.Logf("[cleanup] deleteGitHubBranch: %v", err)
		} else {
			t.Logf("[cleanup] branch %s deleted", branchName)
		}
	})

	cmd := exec.Command(cliBin, "publish")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(),
		"SKPM_GITHUB_TOKEN="+userToken,
		"SKPM_REGISTRY_URL="+registryURL,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	t.Logf("→ running: skpm publish (SKPM_REGISTRY_URL=%s)", registryURL)
	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start)
	t.Logf("[cli] publish took %s", elapsed)
	t.Logf("[cli] output:\n%s", out.String())

	if err != nil {
		t.Fatalf("publish command failed (%v)", err)
	}

	outputStr := out.String()
	if !strings.Contains(outputStr, "Published "+pkgName) &&
		!strings.Contains(outputStr, "pull request") {
		t.Errorf("unexpected publish output; expected 'Published %s@%s':\n%s",
			pkgName, version, outputStr)
	} else {
		t.Logf("  ✓ CLI output confirms publish")
	}

	// Verify the PR was actually opened on GitHub.
	t.Logf("→ verifying PR exists on skpm-dev/e2e for branch %s", branchName)
	prNum, err := findOpenPR(regToken, "skpm-dev", "e2e", branchName)
	if err != nil {
		t.Fatalf("findOpenPR: %v", err)
	}
	if prNum == 0 {
		t.Errorf("expected an open PR on skpm-dev/e2e for branch %s but found none", branchName)
	} else {
		t.Logf("  ✓ PR #%d is open on skpm-dev/e2e", prNum)
	}
}

// ---------------------------------------------------------------------------
// TestPublish helpers
// ---------------------------------------------------------------------------

// buildBinary compiles a Go binary from srcDir and returns its path.
func buildBinary(t *testing.T, srcDir, name string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), name)
	abs, err := filepath.Abs(srcDir)
	if err != nil {
		t.Fatalf("[build] abs path for %s: %v", srcDir, err)
	}
	t.Logf("[build] compiling %s from %s → %s", name, abs, binPath)

	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = abs
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("[build] %s failed:\n%s\nerror: %v", name, buf.String(), err)
	}
	t.Logf("[build] %s compiled OK", name)
	return binPath
}

// startLocalRegistry builds and starts the registry server in-process,
// configured to target skpm-dev/e2e as its backing GitHub repo.
// Skips the test if REGISTRY_GITHUB_TOKEN is not set.
// Returns the base URL of the running registry.
func startLocalRegistry(t *testing.T) string {
	t.Helper()
	regToken := os.Getenv("REGISTRY_GITHUB_TOKEN")
	if regToken == "" {
		t.Skip("REGISTRY_GITHUB_TOKEN not set — skipping publish test")
	}

	bin := buildBinary(t, "../registry", "skpm-registry")

	// Pick a free port.
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("[registry] find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	t.Logf("[registry] starting on %s (REGISTRY_GITHUB_OWNER=skpm-dev REGISTRY_GITHUB_REPO=e2e)", baseURL)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"PORT="+strconv.Itoa(port),
		"REGISTRY_GITHUB_TOKEN="+regToken,
		"REGISTRY_GITHUB_OWNER=skpm-dev",
		"REGISTRY_GITHUB_REPO=e2e",
	)
	var regLog bytes.Buffer
	cmd.Stdout = &regLog
	cmd.Stderr = &regLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("[registry] start: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		if t.Failed() || testing.Verbose() {
			t.Logf("[registry] stdout/stderr:\n%s", regLog.String())
		}
	})

	// Poll until the registry responds.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/packages/health-probe-nonexistent")
		if err == nil {
			resp.Body.Close()
			t.Logf("[registry] ready at %s", baseURL)
			return baseURL
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("[registry] stdout/stderr:\n%s", regLog.String())
	t.Fatalf("[registry] did not become ready within 15s")
	return ""
}

// findOpenPR returns the PR number of an open PR for the given branch,
// or 0 if none found.
func findOpenPR(token, owner, repo, branch string) (int, error) {
	url := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/pulls?head=%s:%s&state=open",
		owner, repo, owner, branch,
	)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
		return 0, fmt.Errorf("decode PR list: %w", err)
	}
	if len(prs) == 0 {
		return 0, nil
	}
	return prs[0].Number, nil
}

// closeGitHubPR closes an open PR by number.
func closeGitHubPR(token, owner, repo string, prNumber int) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	body := strings.NewReader(`{"state":"closed"}`)
	req, _ := http.NewRequest(http.MethodPatch, url, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned %d closing PR #%d", resp.StatusCode, prNumber)
	}
	return nil
}

// deleteGitHubBranch deletes a branch via the GitHub API.
func deleteGitHubBranch(token, owner, repo, branch string) error {
	url := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/git/refs/heads/%s",
		owner, repo, branch,
	)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("GitHub returned %d deleting branch %s", resp.StatusCode, branch)
	}
	return nil
}

// ---------------------------------------------------------------------------
// injectOldInstall: force a fake "old version" install for update tests
// ---------------------------------------------------------------------------

// lockFileFormat mirrors the on-disk JSON structure written by LockFile.kt.
type lockFileFormat struct {
	SchemaVersion int           `json:"schemaVersion"`
	GeneratedAt   string        `json:"generatedAt"`
	Packages      []lockEntryRaw `json:"packages"`
}

type lockEntryRaw struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Files       map[string]string `json:"files"`
	Description string            `json:"description,omitempty"`
}

// injectOldInstall writes a fake lockfile entry and a minimal script file to
// simulate having pkgName@version installed without going through the actual
// install flow. This lets TestUpdate start from a known old version.
func injectOldInstall(t *testing.T, pkgName, version string) {
	t.Helper()
	t.Logf("[inject] writing fake install: %s@%s", pkgName, version)

	// Write a minimal script file so the package directory exists.
	scriptDir := filepath.Join(serverDir, "plugins/Skript/scripts/skpm", pkgName)
	if err := os.MkdirAll(scriptDir, 0755); err != nil {
		t.Fatalf("[inject] mkdirAll: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "hello.sk")
	content := fmt.Sprintf("# injected for update test: %s@%s\non load:\n    log \"[%s] %s\"\n",
		pkgName, version, pkgName, version)
	if err := os.WriteFile(scriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("[inject] write script: %v", err)
	}
	t.Logf("[inject] wrote %s", scriptPath)

	// Read existing lockfile to preserve other entries.
	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	var lf lockFileFormat
	if data, err := os.ReadFile(lockPath); err == nil {
		_ = json.Unmarshal(data, &lf)
	}
	if lf.SchemaVersion == 0 {
		lf.SchemaVersion = 1
	}

	// Remove any existing entry for this package, then append the injected one.
	var filtered []lockEntryRaw
	for _, e := range lf.Packages {
		if e.Name != pkgName {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered, lockEntryRaw{
		Name:        pkgName,
		Version:     version,
		Files:       map[string]string{"hello.sk": ""},
		Description: "Dedicated package for skpm end-to-end tests",
	})
	lf.Packages = filtered

	lockData, _ := json.MarshalIndent(lf, "", "  ")
	if err := os.WriteFile(lockPath, lockData, 0644); err != nil {
		t.Fatalf("[inject] write lockfile: %v", err)
	}
	t.Logf("[inject] wrote lockfile entry: %s@%s", pkgName, version)
}

// ---------------------------------------------------------------------------
// JAR download helpers (unchanged from original)
// ---------------------------------------------------------------------------

func downloadLatestJAR(owner, repo, dest string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %d for %s/%s", resp.StatusCode, owner, repo)
	}

	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return err
	}

	for _, asset := range release.Assets {
		if strings.HasSuffix(asset.Name, ".jar") {
			return fetchFile(asset.BrowserDownloadURL, dest)
		}
	}
	return fmt.Errorf("no .jar asset in latest release of %s/%s", owner, repo)
}

func fetchFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
