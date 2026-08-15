package browser

import (
	"fmt"
	"os"
	"bufio"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/input"

	"gscolar-scraper/internal/notify"
	"gscolar-scraper/internal/parse"
)

// Scholar element selectors. The search box, results and pagination selectors
// are stable Scholar IDs/classes; the custom-year-range selectors depend on
// the sidebar markup and are checked/refined during the live-crawl milestone.
const (
	selScholarPage  = "https://scholar.google.com/"
	selSearchInput  = `#gs_hdr_tsi`
	selSearchButton = `#gs_hdr_tsb`
	selResults      = `.gs_r.gs_or`
	selNext         = `#gs_n a[href*="start="] span.gs_ico_nav_next`

	// Custom year range (sidebar): trigger link and From/To inputs. There is no
	// reliable apply-button selector across Scholar versions; pressing Enter in
	// the "To" field is Scholar's accepted shortcut and is used as the apply.
	selYearTrigger = `#gs_asd_tsb`
	selYearFrom    = `#gs_asd_ylo`
	selYearTo      = `#gs_asd_yhi`
)

// Search runs the full navigation recipe for one task: open Scholar, type the
// query in the search box, optionally restrict to a year range via the
// sidebar's "Custom range" fields, and wait for results.
func (b *Browser) Search(query string, yearFrom, yearTo int) error {
	if err := b.page.Navigate(selScholarPage); err != nil {
		return err
	}
	// The Scholar homepage has no results to wait for — wait for the search box
	// (present on both the homepage and the results pages) instead.
	if err := b.waitForSearchBox(60 * time.Second); err != nil {
		return err
	}

	searchInput, err := b.page.Element(selSearchInput)
	if err != nil {
		return fmt.Errorf("search input not found: %w", err)
	}
	if err := searchInput.SelectAllText(); err != nil {
		return err
	}
	if err := b.humanType(searchInput, query); err != nil {
		return err
	}
	// Click the search button (not just Enter) for a more natural interaction.
	btn, err := b.page.Element(selSearchButton)
	if err == nil {
		_ = b.humanClick(btn)
	} else {
		_ = b.page.Keyboard.Press(input.Enter)
	}
	if err := b.WaitForResults(60 * time.Second); err != nil {
		return err
	}

	if yearFrom > 0 || yearTo > 0 {
		if err := b.setYearRange(yearFrom, yearTo); err != nil {
			return err
		}
	}
	return nil
}

// setYearRange opens the sidebar "Custom range" fields and applies a year
// window. It tolerates the markup varying by Scholar version by trying a few
// selectors; failures here should be surfaced as a selector that needs
// updating, not silently swallowed.
func (b *Browser) setYearRange(yearFrom, yearTo int) error {
	trigger, err := b.page.Element(selYearTrigger)
	if err != nil {
		// Some Scholar versions only show the year fields after a click on a
		// sidebar link with a "Custom" label; try a broader search.
		trigger, err = b.page.Element(`a[onclick*="gs_asd"], a#gs_asd_tsb`)
		if err != nil {
			return fmt.Errorf("custom-range trigger not found (check selector): %w", err)
		}
	}
	if err := b.humanClick(trigger); err != nil {
		return err
	}
	b.sleepRange(600, 1500)

	from, err := b.page.Element(selYearFrom)
	if err != nil {
		return fmt.Errorf("year-from field not found (check selector): %w", err)
	}
	to, err := b.page.Element(selYearTo)
	if err != nil {
		return fmt.Errorf("year-to field not found (check selector): %w", err)
	}

	if err := from.SelectAllText(); err != nil {
		return err
	}
	if err := b.humanType(from, strconv.Itoa(yearFrom)); err != nil {
		return err
	}
	if err := to.SelectAllText(); err != nil {
		return err
	}
	if err := b.humanType(to, strconv.Itoa(yearTo)); err != nil {
		return err
	}

	// Apply the range by pressing Enter in the "To" field, which Scholar honors
	// as the accept action for the custom-range form.
	if err := b.page.Keyboard.Press(input.Enter); err != nil {
		return err
	}
	if err := b.WaitForResults(60 * time.Second); err != nil {
		return err
	}
	return nil
}

// ClickNext follows the results pagination "Next" link (clicked via the icon,
// not by URL editing).
func (b *Browser) ClickNext() error {
	icon, err := b.page.Element(selNext)
	if err != nil {
		return fmt.Errorf("next link not found: %w", err)
	}
	link, err := icon.Parent() // the <a href="...start=..."> anchor
	if err != nil {
		return err
	}
	if err := b.humanClick(link); err != nil {
		return err
	}
	return b.WaitForResults(60 * time.Second)
}

// Reload reloads the current page (used after a CAPTCHA is solved).
func (b *Browser) Reload() error {
	if err := b.page.Reload(); err != nil {
		return err
	}
	return b.WaitForResults(60 * time.Second)
}

// Content returns the full raw HTML of the current page.
func (b *Browser) Content() (string, error) {
	return b.page.HTML()
}

// URL returns the current page URL.
func (b *Browser) URL() string {
	info, err := b.page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

// IsBlocked reports whether the current page is a CAPTCHA/block page, based on
// both the DOM markers and the URL.
func (b *Browser) IsBlocked() bool {
	html, err := b.page.HTML()
	if err != nil {
		return false
	}
	if parse.IsBlocked(html) {
		return true
	}
	u := b.URL()
	return strings.Contains(u, "/sorry/") || strings.Contains(u, "captcha")
}

// WaitForResults waits until Scholar shows either result rows or the results
// header (#gs_ab_md), resolving CAPTCHAs and Google sign-in walls as they
// appear (each manual resolution restarts the deadline).
func (b *Browser) WaitForResults(timeout time.Duration) error {
	return b.waitForCondition(timeout, "no Scholar results", func() bool {
		n, err := b.page.Eval(`document.querySelectorAll('.gs_r.gs_or, #gs_ab_md').length`)
		return err == nil && n.Value.Int() > 0
	})
}

// waitForSearchBox waits until Scholar's search box is present (homepage or
// results page), resolving CAPTCHAs and Google sign-in walls as they appear.
func (b *Browser) waitForSearchBox(timeout time.Duration) error {
	return b.waitForCondition(timeout, "Scholar search box not found", func() bool {
		_, err := b.page.Element(selSearchInput)
		return err == nil
	})
}

// waitForCondition polls fn until it returns true or the deadline passes. If a
// CAPTCHA or a Google sign-in wall interrupts, it blocks for the user to clear
// it (the same "solve and press Enter" flow as WaitForCaptchaResolved) and
// restarts the deadline, so user time doesn't count against the timeout.
func (b *Browser) waitForCondition(timeout time.Duration, what string, fn func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if b.IsBlocked() {
			return ErrBlocked
		}
		if b.isLoginWall() {
			if err := b.WaitForLoginResolved(); err != nil {
				return err
			}
			deadline = time.Now().Add(timeout)
			continue
		}
		if fn() {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("%s within %v (URL=%s)", what, timeout, b.URL())
}

// isLoginWall reports whether the current page is a Google wall — Scholar
// bouncing the session to accounts.google.com to force a login, or a consent
// page — rather than a Scholar page.
func (b *Browser) isLoginWall() bool {
	u := b.URL()
	return strings.Contains(u, "accounts.google.com") ||
		strings.Contains(u, "ServiceLogin") ||
		strings.Contains(u, "/signin/") ||
		strings.Contains(u, "consent.google.com")
}

// WaitForLoginResolved blocks until the user signs in to Google in the browser
// window, then waits like a human returning from a break and reloads the page
// to resume. It mirrors WaitForCaptchaResolved.
func (b *Browser) WaitForLoginResolved() error {
	msg := "Google is showing a sign-in page. Sign in to your Google account " +
		"in the browser window, then return here and press Enter to continue."
	_ = notify.Captcha("Scholar Scraper", msg)
	fmt.Println("\n" + msg)

	bufio.NewReader(os.Stdin).ReadString('\n')

	// Simulate a human returning from a break, then reload and re-check.
	b.sleepRange(b.cfg.Timing.AfterCaptchaMS[0], b.cfg.Timing.AfterCaptchaMS[1])
	return b.page.Reload()
}

// WaitForCaptchaResolved blocks until the user solves the CAPTCHA and presses
// Enter in the terminal, then waits like a human returning from a break and
// reloads the page to resume.
func (b *Browser) WaitForCaptchaResolved() error {
	msg := "CAPTCHA or block page detected. Solve it in the browser window, " +
		"then return here and press Enter to continue."
	_ = notify.Captcha("Scholar Scraper", msg)
	fmt.Println("\n" + msg)

	// Spec §4.6: block until the user signals continuation.
	bufio.NewReader(os.Stdin).ReadString('\n')

	// Simulate a human returning from a break, then reload and wait.
	b.sleepRange(b.cfg.Timing.AfterCaptchaMS[0], b.cfg.Timing.AfterCaptchaMS[1])
	if err := b.Reload(); err != nil && err != ErrBlocked {
		return err
	}
	return nil
}
