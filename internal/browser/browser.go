// Package browser drives the user's real Chrome/Edge via CDP and emulates
// human input. Every Scholar interaction goes through these helpers: typed
// text, curved mouse movements, wheel scrolling and jittered delays.
package browser

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"

	"gscolar-scraper/internal/config"
)

// ErrBlocked is returned when a page looks like a CAPTCHA / block page.
var ErrBlocked = errors.New("captcha or block page detected")

// Browser wraps the rod browser + page together with the human-emulation
// state (current mouse position, adaptive throttle multiplier).
type Browser struct {
	cfg *config.Config

	browser *rod.Browser
	page    *rod.Page

	mu      sync.Mutex
	mouseX  float64
	mouseY  float64
	throttle float64
}

// New creates a Browser handle bound to cfg but does not connect yet.
func New(cfg *config.Config) *Browser {
	return &Browser{
		cfg:      cfg,
		mouseX:   400,
		mouseY:   300,
		throttle: 1.0,
	}
}

// Connect attaches to an already-running browser on the debug port if one is
// listening, otherwise launches the user's browser with a fresh debugging
// session. Chrome/Edge refuse --remote-debugging-port on their default data
// dir, so the browser runs against a dedicated non-default profile directory
// seeded once from a copy of the user's real profile (logins/cookies carry
// over). Never headless, never a sandbox.
func (b *Browser) Connect() error {
	port := b.cfg.Browser.DebugPort
	if port == 0 {
		port = 9222
	}

	// 1. Try to attach to an instance already listening on the debug port.
	ctrl := fmt.Sprintf("http://127.0.0.1:%d", port)
	if b.portOpen(port) {
		browser := rod.New().ControlURL(ctrl)
		if err := browser.Connect(); err != nil {
			return fmt.Errorf("connect to running browser on %s: %w", ctrl, err)
		}
		b.browser = browser
		return b.newPage()
	}

	// 2. Launch with the user's profile and a fixed debug port. If the user's
	// browser is already open, Chrome will ignore the debug flag and hand the
	// launch off to the running process — so ask them to close it first.
	fmt.Println("No browser found on the debug port.")
	fmt.Println("If your Chrome/Edge is currently open, please close it now —")
	fmt.Println("the scraper must launch it with remote debugging on your profile.")
	b.sleepRange(4000, 7000)

	l := launcher.New()
	// rod wraps the browser in its "leakless" helper by default so it can
	// force-kill the browser if this process dies. Windows Defender flags that
	// helper (leakless.exe) as potentially unwanted software, and we want the
	// browser to stay open after the scraper exits anyway — so disable it.
	l.Leakless(false)
	l.Headless(b.cfg.Browser.Headless)
	l.Set("--remote-debugging-port", strconv.Itoa(port))
	l.Set("--disable-blink-features", "AutomationControlled")
	if p, err := b.resolveProfileDir(); err == nil && p != "" {
		l.UserDataDir(p)
	}
	if exe, err := b.resolveExecutable(); err == nil && exe != "" {
		l.Bin(exe)
	}

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch browser: %w (close your browser and retry)", err)
	}
	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		return fmt.Errorf("connect to freshly launched browser: %w", err)
	}
	b.browser = browser
	return b.newPage()
}

func (b *Browser) newPage() error {
	// stealth.Page creates a fresh tab and patches the automation telltales
	// (navigator.webdriver and friends) before any page script runs. The user's
	// real profile is the primary fingerprint defense; this is the secondary one.
	page, err := stealth.Page(b.browser)
	if err != nil {
		return err
	}
	b.page = page
	return nil
}

func (b *Browser) portOpen(port int) bool {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// resolveExecutable returns the browser binary to drive.
func (b *Browser) resolveExecutable() (string, error) {
	if b.cfg.Browser.Executable != "" {
		return b.cfg.Browser.Executable, nil
	}
	switch b.resolveKind() {
	case "edge":
		return findEdge(), nil
	default: // chrome: rod's launcher finds Chrome on its own
		return "", nil
	}
}

// resolveKind normalizes cfg.Browser.Kind, auto-detecting Edge vs Chrome when
// unset. resolveExecutable and resolveProfileDir must agree on this so the
// launched binary and the copied profile are the same browser.
func (b *Browser) resolveKind() string {
	if b.cfg.Browser.Kind != "" {
		return b.cfg.Browser.Kind
	}
	if findEdge() != "" {
		return "edge"
	}
	return "chrome"
}

// resolveProfileDir returns the profile directory to hand to the browser.
// Chrome/Edge refuse --remote-debugging-port when pointed at their default data
// dir, so we use a dedicated non-default location and, on first use, seed it
// from a copy of the real profile so the existing session carries over.
func (b *Browser) resolveProfileDir() (string, error) {
	if b.cfg.Browser.ProfileDir != "" {
		return b.cfg.Browser.ProfileDir, nil
	}
	kind := b.resolveKind()
	dst := scrapeProfileDir(kind)
	if dst == "" {
		return "", nil
	}
	if src, err := b.realProfileDir(kind); err == nil && src != "" {
		seedProfile(src, dst)
	}
	return dst, nil
}

// realProfileDir returns the browser's real default profile directory.
func (b *Browser) realProfileDir(kind string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch kind {
	case "edge":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Microsoft", "Edge", "User Data"), nil
		}
		return filepath.Join(home, "Library", "Application Support", "Microsoft Edge"), nil
	default: // chrome
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Google", "Chrome", "User Data"), nil
		}
		if p := filepath.Join(home, ".config", "google-chrome"); dirExists(p) {
			return p, nil
		}
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome"), nil
	}
}

// scrapeProfileDir returns the dedicated non-default profile directory the
// scraper drives, separate from the browser's own data dir.
func scrapeProfileDir(kind string) string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "gscolar", kind+"-profile")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gscolar", kind+"-profile")
	}
	return ""
}

// seedProfile copies the session-bearing files of the user's real profile into
// dst so the scraper's profile carries the existing login state. Best-effort:
// missing or locked files (browser still open) are skipped, and dst is only
// seeded once.
func seedProfile(src, dst string) {
	if dirExists(dst) {
		return
	}
	_ = os.MkdirAll(dst, 0o755)

	for _, rel := range []string{
		"Local State",
		"Default/Preferences",
		"Default/Secure Preferences",
		"Default/Cookies",
		"Default/Cookies-journal",
		"Default/Network/Cookies",
		"Default/Network/Cookies-journal",
		"Default/Login Data",
		"Default/Login Data-journal",
	} {
		sp := filepath.Join(src, filepath.FromSlash(rel))
		if !fileExists(sp) {
			continue
		}
		dp := filepath.Join(dst, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(dp), 0o755)
		if err := copyFile(sp, dp); err != nil {
			fmt.Printf("seed profile: copy %s: %v\n", rel, err)
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func findEdge() string {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, c := range candidates {
		if dirExists(c) {
			return c
		}
	}
	if p, err := exec.LookPath("microsoft-edge"); err == nil {
		return p
	}
	return ""
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Page exposes the underlying rod page (used by the crawl loop for Content()).
func (b *Browser) Page() *rod.Page { return b.page }

// Close shuts the page down (the browser window stays open for the user).
func (b *Browser) Close() error {
	if b.page != nil {
		_ = b.page.Close()
	}
	return nil
}
