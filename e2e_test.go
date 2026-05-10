package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
)

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

	fmt.Println("→ Downloading SKPM.jar...")
	if err := downloadLatestJAR("skpm-dev", "plugin", filepath.Join(pluginsDir, "SKPM.jar")); err != nil {
		return fmt.Errorf("download SKPM: %w", err)
	}

	fmt.Println("→ Downloading Skript.jar...")
	if err := downloadLatestJAR("SkriptLang", "Skript", filepath.Join(pluginsDir, "Skript.jar")); err != nil {
		return fmt.Errorf("download Skript: %w", err)
	}

	fmt.Println("→ Starting server...")
	up := exec.Command("docker", "compose", "up", "-d")
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	fmt.Println("→ Waiting for server...")
	if err := waitForRCON(3 * time.Minute); err != nil {
		return err
	}

	// Give Skript time to finish loading after RCON is available
	fmt.Println("→ Waiting for Skript to load...")
	time.Sleep(15 * time.Second)

	return nil
}

func teardown() {
	fmt.Println("→ Tearing down...")
	exec.Command("docker", "compose", "down", "-v").Run()
	os.RemoveAll(serverDir)
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

func connect(t *testing.T) *rcon.Conn {
	t.Helper()
	conn, err := rcon.Dial(rconAddr, rconPassword)
	if err != nil {
		t.Fatalf("RCON connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
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

// TestInstall installs test-economy and verifies the script file and lockfile.
func TestInstall(t *testing.T) {
	conn := connect(t)

	if _, err := conn.Execute("skpm install test-economy"); err != nil {
		t.Fatalf("install command: %v", err)
	}

	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/test-economy/economy.sk")
	if err := waitForFile(scriptPath, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if !strings.Contains(string(lock), "test-economy") {
		t.Fatalf("lockfile missing test-economy entry:\n%s", lock)
	}
}

// TestRemove removes test-economy and verifies cleanup.
func TestRemove(t *testing.T) {
	conn := connect(t)

	scriptPath := filepath.Join(serverDir, "plugins/Skript/scripts/skpm/test-economy/economy.sk")

	// Ensure installed first
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		if _, err := conn.Execute("skpm install test-economy"); err != nil {
			t.Fatalf("install command: %v", err)
		}
		if err := waitForFile(scriptPath, 30*time.Second); err != nil {
			t.Fatalf("pre-condition: %v", err)
		}
	}

	if _, err := conn.Execute("skpm remove test-economy"); err != nil {
		t.Fatalf("remove command: %v", err)
	}

	if err := waitForFileGone(scriptPath, 15*time.Second); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(serverDir, "plugins/SKPM/skript.lock")
	lock, err := os.ReadFile(lockPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read lockfile: %v", err)
	}
	if strings.Contains(string(lock), "test-economy") {
		t.Fatalf("lockfile still contains test-economy after remove:\n%s", lock)
	}
}

// downloadLatestJAR fetches the first .jar asset from the latest GitHub release.
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
