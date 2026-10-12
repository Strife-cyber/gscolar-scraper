package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/go-rod/rod"

	"gscolar-scraper/internal/config"
	"gscolar-scraper/internal/parse"
)

// TestInspectLive is a manual diagnostic (skipped unless GSC_LIVE_INSPECT=1).
// It attaches to the running browser on the debug port, inspects every open
// Scholar tab, and reports what WaitForResults / applyCitationFilter / Parse
// would see on it. Run with:
//
//	GSC_LIVE_INSPECT=1 go test ./internal/browser -run TestInspectLive -v
func TestInspectLive(t *testing.T) {
	if os.Getenv("GSC_LIVE_INSPECT") == "" {
		t.Skip("set GSC_LIVE_INSPECT=1 to run")
	}
	cfg, err := config.Load("../../config.json")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", cfg.Browser.DebugPort))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	rb := rod.New().ControlURL(v.WS)
	if err := rb.Connect(); err != nil {
		t.Fatal(err)
	}
	pages, err := rb.Pages()
	if err != nil {
		t.Fatal(err)
	}
	for _, pg := range pages {
		info, err := pg.Info()
		if err != nil {
			continue
		}
		fmt.Printf("\n=== tab: %s\n    %s\n", info.Title, info.URL)
		ro, err := pg.Eval(`() => {
			const all = [...document.querySelectorAll('a.gs_in_cb[href*="as_vis=1"][aria-checked="true"]')];
			const vis = e => {
				if (!e) return null;
				const r = e.getBoundingClientRect();
				const cs = getComputedStyle(e);
				return {w: r.width, h: r.height, x: r.x, y: r.y,
				        disp: cs.display, vis: cs.visibility, op: cs.opacity,
				        offP: !!e.offsetParent,
				        inDropdown: !!e.closest('#gs_ab_md_dd, .gs_bdy_sb_it, [style*="display: none"], [style*="display:none"]')};
			};
			return {
			rows:      document.querySelectorAll('.gs_r.gs_or').length,
			md:        !!document.querySelector('#gs_ab_md'),
			mdText:    (document.querySelector('#gs_ab_md')||{}).innerText || '',
			shell:     !!document.querySelector('#gs_res_ccl'),
			cbs:       document.querySelectorAll('.gs_in_cb').length,
			citesInc:  all.length,
			citesVis:  all.map(vis),
			nextAny:   !!document.querySelector('#gs_n a[href*="start="]'),
			nextIcon:  !!document.querySelector('#gs_n a[href*="start="] span.gs_ico_nav_next'),
			searchBox: !!document.querySelector('#gs_hdr_tsi'),
			captcha:   document.querySelectorAll('form#captcha-form, .g-recaptcha, #captcha, div#sorry').length,
			ready:     document.readyState,
			hidden:    document.hidden,
			visState:  document.visibilityState,
			};
		}`)
		if err != nil {
			fmt.Printf("    eval failed: %v\n", err)
			continue
		}
		for _, k := range []string{"rows", "md", "mdText", "shell", "cbs", "citesInc", "citesVis", "nextAny", "nextIcon", "searchBox", "captcha", "ready", "hidden", "visState"} {
			fmt.Printf("    %-10s = %v\n", k, ro.Value.Get(k))
		}
		html, err := pg.HTML()
		if err != nil {
			fmt.Printf("    HTML() failed: %v\n", err)
			continue
		}
		p, err := parse.Parse(html)
		if err != nil {
			fmt.Printf("    parse failed: %v\n", err)
			continue
		}
		fmt.Printf("    parse: items=%d count=%d hasCount=%v hasNext=%v blocked=%v noResults=%v\n",
			len(p.Items), p.Count, p.HasCount, p.HasNext, p.Blocked, p.NoResults)
	}
}
