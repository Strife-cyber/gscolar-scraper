// Package browser drives the user's real Chrome/Edge via CDP and emulates
// human input. Every Scholar interaction goes through these helpers: typed
// text, curved mouse movements, wheel scrolling and jittered delays.
package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
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
//
// Both paths call NoDefaultDevice(). rod otherwise emulates
// devices.LaptopWithMDPIScreen on every new page, and that emulation ends in
// SetUserAgent with a hardcoded "Macintosh ... Chrome/114" string. Overriding
// the UA does NOT override the Client Hints headers (Sec-CH-UA-Platform,
// Sec-CH-UA-Full-Version-List) the real binary emits, so the browser announces
// macOS/Chrome 114 and Windows/Chrome 154 in the same request — an OS and forty
// versions apart. Scholar answers that contradiction with a soft block: no
// CAPTCHA, no error, just degraded pages whose "About N results" count swings
// by an order of magnitude between identical queries (313, 197, then 1940 for
// the same ICRA 2004-2005 search). The override lives on the CDP session, which
// is why the count snaps back to a stable, correct value the moment the scraper
// detaches.
func (b *Browser) Connect() error {
	port := b.cfg.Browser.DebugPort
	if port == 0 {
		port = 9222
	}

	// 1. Try to attach to an instance already listening on the debug port.
	if b.portOpen(port) {
		wsURL, err := browserWebSocketURL(port)
		if err != nil {
			return fmt.Errorf("resolve running browser endpoint on port %d: %w", port, err)
		}
		browser := rod.New().ControlURL(wsURL).NoDefaultDevice()
		if err := browser.Connect(); err != nil {
			return fmt.Errorf("connect to running browser on %s: %w", wsURL, err)
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
	// rod's launcher sets --enable-automation by default. It is the single most
	// explicit "I am a bot" signal a Chromium can send (it drives the automation
	// infobar and a batch of navigator flags), which flatly contradicts driving
	// a real profile to look human. Drop it.
	l.Delete("enable-automation")
	// Force the UI language. Scholar picks hl= from these, and hl=fr makes it
	// treat "and" as a mandatory query word rather than a stopword — that alone
	// slashes the counts of every venue whose name contains it (ICRA, KDD,
	// AAMAS…). See BrowserConfig.Locale.
	locale := b.locale()
	l.Set("--lang", locale)
	l.Set("--accept-lang", acceptLanguage(locale))
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
	browser := rod.New().ControlURL(url).NoDefaultDevice()
	if err := browser.Connect(); err != nil {
		return fmt.Errorf("connect to freshly launched browser: %w", err)
	}
	b.browser = browser
	return b.newPage()
}

func (b *Browser) newPage() error {
	// stealth.Page patches the automation telltales (navigator.webdriver and
	// friends) before any page script runs. It is OFF by default and must stay
	// that way for Scholar: those same patches break Scholar's own browser
	// sniffer, which then serves a degraded page — unstable "About N results"
	// counts and missing pagination. The real profile is the fingerprint
	// defense; this patching actively works against it here.
	var (
		page *rod.Page
		err  error
	)
	if b.cfg.Browser.Stealth {
		page, err = stealth.Page(b.browser)
	} else {
		page, err = b.browser.Page(proto.TargetCreateTarget{})
	}
	if err != nil {
		return err
	}
	b.page = page
	return b.applyLocale()
}

// locale returns the configured UI locale, defaulting to en-US.
func (b *Browser) locale() string {
	if l := b.cfg.Browser.Locale; l != "" {
		return l
	}
	return "en-US"
}

// acceptLanguage builds the Accept-Language header value for a locale
// ("en-US" -> "en-US,en;q=0.9").
func acceptLanguage(locale string) string {
	short := locale
	if i := strings.Index(locale, "-"); i > 0 {
		short = locale[:i]
	}
	return fmt.Sprintf("%s,%s;q=0.9", locale, short)
}

// applyLocale pins the page's language three ways, because no single one is
// reliable: the Accept-Language header (what Scholar reads to pick hl=), and
// Google's own PREF cookie (what it consults to remember a UI language, and
// which otherwise wins over the header from a French IP). The --lang /
// --accept-lang switches set at launch are the third.
func (b *Browser) applyLocale() error {
	locale := b.locale()
	if _, err := b.page.SetExtraHeaders([]string{"Accept-Language", acceptLanguage(locale)}); err != nil {
		return fmt.Errorf("set Accept-Language: %w", err)
	}
	short := locale
	if i := strings.Index(locale, "-"); i > 0 {
		short = locale[:i]
	}
	region := "US"
	if i := strings.Index(locale, "-"); i > 0 {
		region = locale[i+1:]
	}
	// Best-effort: a cookie rejection must not abort the crawl.
	if err := b.browser.SetCookies([]*proto.NetworkCookieParam{{
		Name:     "PREF",
		Value:    fmt.Sprintf("hl=%s&gl=%s", short, region),
		Domain:   ".google.com",
		Path:     "/",
		Secure:   true,
		SameSite: proto.NetworkCookieSameSiteLax,
	}}); err != nil {
		fmt.Printf("seed PREF cookie: %v\n", err)
	}
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

// browserWebSocketURL resolves the CDP browser endpoint of an already-running
// browser on port. rod's Connect with a plain "http://host:port" control URL
// dials the websocket at that URL's root path, which Chrome answers with a 404;
// the real endpoint is ws://host:port/devtools/browser/<id>, advertised in
// /json/version. Resolving it lets the scraper attach to a browser the user
// left open (the scraper keeps the window alive by design) instead of forcing a
// relaunch and its "close your browser first" dance.
func browserWebSocketURL(port int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.WS == "" {
		return "", fmt.Errorf("no webSocketDebuggerUrl in /json/version")
	}
	return v.WS, nil
}

// resolveExecutable returns the browser binary to drive. It must return a real,
// installed browser whenever one exists: leaving this empty lets rod's launcher
// fall back to downloading its own ~600 MB Chromium, and that bare binary — no
// history, no extensions, default everything — is precisely the fingerprint the
// "drive the user's real browser" design exists to avoid.
func (b *Browser) resolveExecutable() (string, error) {
	if b.cfg.Browser.Executable != "" {
		return b.cfg.Browser.Executable, nil
	}
	switch b.resolveKind() {
	case "edge":
		return findEdge(), nil
	default:
		return findChrome(), nil
	}
}

// findChrome locates an installed Google Chrome, or "" to let rod decide.
func findChrome() string {
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		candidates = append(candidates, filepath.Join(d, "Google", "Chrome", "Application", "chrome.exe"))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
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
	// fileExists, not dirExists: these are paths to an executable FILE. The
	// original dirExists check could never match, so auto-detection never
	// picked Edge and "kind":"edge" resolved to an empty binary path.
	for _, c := range candidates {
		if fileExists(c) {
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
