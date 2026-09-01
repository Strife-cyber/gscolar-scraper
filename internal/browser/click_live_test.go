package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/config"
)

// TestClickNextLive is a manual diagnostic (skipped unless GSC_LIVE_CLICK_TEST=1).
// It attaches to the running browser on the configured debug port, loads the
// exact page the crawler was stuck on (task 19, 2005 network -neural), and
// checks whether the Next-anchor click actually navigates. Run it with:
//
//	GSC_LIVE_CLICK_TEST=1 go test ./internal/browser -run TestClickNextLive -v
func TestClickNextLive(t *testing.T) {
	if os.Getenv("GSC_LIVE_CLICK_TEST") == "" {
		t.Skip("set GSC_LIVE_CLICK_TEST=1 to run")
	}
	cfg, err := config.Load("../../config.json")
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg)
	// Connect() with ControlURL("http://127.0.0.1:9333") dials the ws root and
	// gets a 404; resolve the real browser websocket from /json/version instead.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", cfg.Browser.DebugPort))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	rb := rod.New().ControlURL(v.WS)
	if err := rb.Connect(); err != nil {
		t.Fatal(err)
	}
	b.browser = rb
	if err := b.newPage(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const pageURL = "https://scholar.google.com/scholar?start=0&q=network+-neural+source:International+source:Conference+source:on+source:Machine+source:Learning&hl=en&as_sdt=0,5&as_ylo=2005&as_yhi=2005"

	load := func() {
		if err := b.page.Navigate(pageURL); err != nil {
			t.Fatal(err)
		}
		if err := b.WaitForResults(60 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	load()
	fmt.Printf("initial URL: %s\n", b.URL())

	icon, err := b.page.Element(selNext)
	if err != nil {
		t.Fatalf("next icon: %v", err)
	}
	link, err := icon.Parent()
	if err != nil {
		t.Fatal(err)
	}

	if shape, err := link.Shape(); err == nil && len(shape.Quads) > 0 {
		q := shape.Quads[0]
		fmt.Printf("next-anchor center: x=%.0f y=%.0f\n", (q[0]+q[2]+q[4]+q[6])/4, (q[1]+q[3]+q[5]+q[7])/4)
	}
	if ro, err := b.page.Eval(`() => ({ w: innerWidth, h: innerHeight, scrollY, docH: document.documentElement.scrollHeight })`); err == nil {
		s := ro.Value
		fmt.Printf("viewport w=%.0f h=%.0f scrollY=%.0f docH=%.0f\n",
			s.Get("w").Num(), s.Get("h").Num(), s.Get("scrollY").Num(), s.Get("docH").Num())
	}

	fmt.Println("\n--- TEST 1: current humanClick on next anchor (no scroll) ---")
	if err := b.humanClick(link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(8 * time.Second)
	fmt.Printf("URL after current-style click: %s\n", b.URL())

	fmt.Println("\n--- TEST 2: ScrollIntoViewIfNeeded, then humanClick ---")
	load()
	icon2, err := b.page.Element(selNext)
	if err != nil {
		t.Fatal(err)
	}
	link2, err := icon2.Parent()
	if err != nil {
		t.Fatal(err)
	}
	if err := link2.ScrollIntoView(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := b.humanClick(link2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(8 * time.Second)
	fmt.Printf("URL after scroll+click: %s\n", b.URL())
}
