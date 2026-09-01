# samples/ — recorded Scholar pages

Each file is a real page captured during scraping, so selectors and the
`internal/parse` logic can be tested offline. The three pieces you pasted (the
header with the search box, the hamburger drawer, and the advanced-search
dialog) are all in `opened.html`.

| File          | Page                                     | What it's for                                                    |
|---------------|------------------------------------------|------------------------------------------------------------------|
| `base.html`   | Scholar homepage (`?hl=fr`, not signed in)| search box `#gs_hdr_tsi`, hamburger `#gs_hdr_mnu`, homepage drawer link `#gs_hp_drw_adv`, hidden `#gs_asd` dialog |
| `opened.html` | results page, query `ernational Conference on Machine Learning"`, drawer + advanced-search dialog open | the mangled-query case; `#gs_res_drw_adv`, full `#gs_asd` form (`#gs_asd_q`, `#gs_asd_eq`, `#gs_asd_pub`, `#gs_asd_ylo`, `#gs_asd_yhi`, `#gs_asd_psb`), pagination `#gs_n` |
| `side-open.html` | signed-in homepage (`?hl=en`) with the hamburger drawer already open | `#gs_hdr_drw` with `.gs_vis`, drawer link `#gs_hp_drw_adv` at its settled position (x≈114); the drawer slides in over ~150-300ms from `translate(-100%,0)`, so clicks must wait for it to settle |
| `normal.html` | results page, `source:AAAI`              | result rows `.gs_r.gs_or`, count header `#gs_ab_md`              |
| `empty.html`  | no-results page, `source:AAAI`           | no `.gs_r.gs_or`, no `#gs_ab_md`                                 |
| `captcha.html`| Google CAPTCHA / block page              | `form#captcha-form`, `.g-recaptcha`, `#g-recaptcha-response`     |

Key selectors the scraper relies on (see `internal/browser/scholar.go`):

- Homepage search box: `#gs_hdr_tsi`
- Hamburger menu: `#gs_hdr_mnu`, drawer `#gs_hdr_drw` (visible class `.gs_vis`)
- Advanced-search drawer link: `#gs_hp_drw_adv` (homepage) / `#gs_res_drw_adv` (results)
- Advanced-search dialog: `#gs_asd` (visible class `.gs_vis`)
  - "with all the words": `#gs_asd_q`
  - "without the words": `#gs_asd_eq`
  - "Return articles published in" (= `source:`): `#gs_asd_pub`
  - year range: `#gs_asd_ylo` / `#gs_asd_yhi`
  - Search button: `#gs_asd_psb`
- Result rows: `.gs_r.gs_or`; results header `#gs_ab_md`
- Next page: `#gs_n a[href*="start="] span.gs_ico_nav_next`
