// Package notify wraps desktop notifications with a terminal fallback.
package notify

import (
	"fmt"
	"os"

	"github.com/gen2brain/beeep"
)

// Captcha sends a desktop notification prompting the user to solve a CAPTCHA,
// and also prints to stdout. On platforms without a notification daemon the
// terminal message (plus a bell) is the fallback.
func Captcha(title, message string) error {
	err := beeep.Notify(title, message, "")
	if err != nil {
		// Fall back to a visible terminal alert; never let notification
		// failures stop the crawl.
		fmt.Fprintf(os.Stderr, "\a\n*** %s ***\n%s\n", title, message)
	}
	return nil
}
