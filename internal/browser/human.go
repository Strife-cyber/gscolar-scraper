package browser

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// rngInt returns a uniform random int in [min, max] (inclusive).
func (b *Browser) rngInt(min, max int) int {
	if max < min {
		min, max = max, min
	}
	return min + rand.IntN(max-min+1)
}

// sleepRange sleeps a jittered delay drawn from [min, max] ms, scaled by the
// current adaptive-throttle multiplier.
func (b *Browser) sleepRange(min, max int) {
	d := b.rngInt(min, max)
	d = int(float64(d) * b.throttle)
	time.Sleep(time.Duration(d) * time.Millisecond)
}

// PauseBetweenPages sleeps the configured between-pages delay.
func (b *Browser) PauseBetweenPages() {
	b.sleepRange(b.cfg.Timing.BetweenPagesMS[0], b.cfg.Timing.BetweenPagesMS[1])
}

// PauseBetweenSearches sleeps the configured between-searches delay.
func (b *Browser) PauseBetweenSearches() {
	b.sleepRange(b.cfg.Timing.BetweenSearchesMS[0], b.cfg.Timing.BetweenSearchesMS[1])
}

// PausePlanning sleeps the (shorter) between-searches delay used during the
// --plan phase, which is lighter than full crawling but still human-paced.
func (b *Browser) PausePlanning() {
	b.sleepRange(b.cfg.Timing.PlanBetweenSearchesMS[0], b.cfg.Timing.PlanBetweenSearchesMS[1])
}

// IncreaseThrottle grows the delay multiplier after a CAPTCHA so the crawl
// slows down and becomes less likely to trip the block again.
func (b *Browser) IncreaseThrottle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.throttle = math.Min(b.throttle+b.cfg.AdaptiveThrottle.Step, b.cfg.AdaptiveThrottle.Max)
}

// Throttle exposes the current multiplier.
func (b *Browser) Throttle() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.throttle
}

// ---------------------------------------------------------------------------
// Typing
// ---------------------------------------------------------------------------

// humanType types text into el character by character with a randomized delay
// per keypress (the spec's 80-250ms), occasionally pausing like a human does.
func (b *Browser) humanType(el *rod.Element, text string) error {
	if err := el.Focus(); err != nil {
		return err
	}
	for _, r := range text {
		if err := b.page.Keyboard.Type(input.Key(r)); err != nil {
			return err
		}
		b.sleepRange(b.cfg.Timing.TypingCharMS[0], b.cfg.Timing.TypingCharMS[1])
		// Occasional mid-word pause, as if thinking.
		if rand.Float64() < 0.04 {
			b.sleepRange(300, 900)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

// elementCenter returns the CSS-pixel center of an element.
func elementCenter(el *rod.Element) (x, y float64, err error) {
	shape, err := el.Shape()
	if err != nil {
		return 0, 0, err
	}
	if len(shape.Quads) == 0 {
		return 0, 0, fmt.Errorf("element has no bounding quad")
	}
	q := shape.Quads[0] // []float64: x1,y1,x2,y2,x3,y3,x4,y4
	return (q[0] + q[2] + q[4] + q[6]) / 4, (q[1] + q[3] + q[5] + q[7]) / 4, nil
}

// humanMoveTo moves the mouse from its current tracked position to (tx, ty)
// along a quadratic Bézier curve (a slight perpendicular arc), with small
// per-waypoint pauses. Each MoveTo(.., 1) advances in one step, so the curve
// is composed of our own waypoints.
func (b *Browser) humanMoveTo(tx, ty float64) error {
	b.mu.Lock()
	sx, sy := b.mouseX, b.mouseY
	b.mu.Unlock()

	// Control point: midpoint of the line, offset perpendicular by a random
	// amount proportional to the distance.
	midX, midY := (sx+tx)/2, (sy+ty)/2
	dx, dy := tx-sx, ty-sy
	dist := math.Hypot(dx, dy)
	if dist < 1 {
		dist = 1
	}
	off := float64(b.rngInt(-18, 18)) / 100.0 * dist
	// perpendicular unit vector
	px, py := -dy/dist, dx/dist
	cx, cy := midX+px*off, midY+py*off

	n := b.rngInt(12, 20)
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		// Quadratic Bézier; ease-out the tail slightly by densifying points.
		tt := t * t
		x := (1-tt)*sx + 2*(1-t)*t*cx + tt*tx
		y := (1-tt)*sy + 2*(1-t)*t*cy + tt*ty
		if err := b.page.Mouse.MoveTo(proto.Point{X: x, Y: y}); err != nil {
			return err
		}
		b.sleepRange(b.cfg.Timing.ClickMoveStepMS[0], b.cfg.Timing.ClickMoveStepMS[1])
	}

	b.mu.Lock()
	b.mouseX, b.mouseY = tx, ty
	b.mu.Unlock()
	return nil
}

// humanClick moves to the element (with a small random offset from its centre)
// and clicks the left button. The element is scrolled into view first: its quad
// is a document-space position, while the mouse events are viewport-space, so
// clicking an element below the fold at its raw quad coordinates misses
// entirely (the pagination Next link, ~2000px down a 20-result page, was the
// casualty — the click silently did nothing and the crawler re-committed the
// same page until the page cap flagged it).
func (b *Browser) humanClick(el *rod.Element) error {
	if err := el.ScrollIntoView(); err != nil {
		return err
	}
	cx, cy, err := elementCenter(el)
	if err != nil {
		return err
	}
	tx := cx + float64(b.rngInt(-4, 4))
	ty := cy + float64(b.rngInt(-4, 4))
	if err := b.humanMoveTo(tx, ty); err != nil {
		return err
	}
	// A beat of hesitation before pressing the button.
	b.sleepRange(50, 160)
	return b.page.Mouse.Click(proto.InputMouseButtonLeft, 1)
}

// humanScroll emits a few randomized wheel events totalling approximately
// deltaY, with pauses between each notch.
func (b *Browser) humanScroll(deltaY int) error {
	steps := b.rngInt(3, 7)
	rem := deltaY
	for i := 0; i < steps && rem != 0; i++ {
		d := rem / (steps - i)
		if err := b.page.Mouse.Scroll(0, float64(d), 1); err != nil {
			return err
		}
		rem -= d
		b.sleepRange(b.cfg.Timing.ScrollStepMS[0], b.cfg.Timing.ScrollStepMS[1])
	}
	return nil
}

// MaybeReadingScroll occasionally scrolls the page up and down, as if reading
// the results, to add behavioural noise.
func (b *Browser) MaybeReadingScroll() error {
	if rand.Float64() >= b.cfg.Timing.ReadScrollProb {
		return nil
	}
	if err := b.humanScroll(b.rngInt(200, 600)); err != nil {
		return err
	}
	b.sleepRange(800, 2000)
	_ = b.humanScroll(-b.rngInt(60, 200))
	return nil
}
