"""Re-queue the tasks whose last stored page was cut off mid-result.

Before the scraper waited for #gs_res_ccl_bot, it sometimes read a results
page while Scholar was still sending it. Such a page has no pagination, so
the crawl took it for the last page and closed the task early.

This lists every task whose LAST stored page is cut off (result rows present,
no #gs_res_ccl_bot) and, with --apply, puts it back to 'pending' with its
resume pointer on that page, so the next -crawl reads it whole and carries on.
'needs_split' and 'superseded' tasks are left alone.

Usage (stop the scraper and back up the database first):
    python scripts/requeue_cut_pages.py scholar.db            # list only
    python scripts/requeue_cut_pages.py scholar.db --apply    # re-queue
"""
import collections
import sqlite3
import sys

REQUEUE = ("completed", "incomplete", "error", "pending")


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    path, apply = sys.argv[1], "--apply" in sys.argv[2:]
    db = sqlite3.connect(path)

    last = {}  # task_id -> (page_number, html) of its highest stored page
    for task_id, page, html in db.execute(
        "SELECT task_id, page_number, html FROM page_html ORDER BY task_id, page_number"
    ):
        last[task_id] = (page, html)

    todo = []
    for task_id, (page, html) in last.items():
        if 'class="gs_r gs_or' not in html or 'id="gs_res_ccl_bot"' in html:
            continue
        row = db.execute(
            "SELECT t.status, c.name, t.year_from, t.year_to, t.query FROM tasks t "
            "JOIN conferences c ON c.id = t.conference_id WHERE t.id = ?",
            (task_id,),
        ).fetchone()
        if row and row[0] in REQUEUE:
            todo.append((task_id, page) + row)

    by_venue = collections.Counter(r[3] for r in todo)
    by_status = collections.Counter(r[2] for r in todo)
    for task_id, page, status, venue, yf, yt, query in todo:
        print(f"task {task_id:5}  {venue:14} {yf}-{yt}  {status:10} cut at page {page:3}  {query[:90]}")
    print(f"\n{len(todo)} tasks ended on a cut-off page; by status {dict(by_status)}")
    print("by venue:", dict(by_venue.most_common()))

    if not apply:
        print("\nNothing changed. Re-run with --apply to re-queue them.")
        return
    with db:
        for task_id, page, *_ in todo:
            db.execute(
                "UPDATE tasks SET status = 'pending', page = ?, error = '' WHERE id = ?",
                (page, task_id),
            )
    print(f"\n{len(todo)} tasks re-queued to pending.")


if __name__ == "__main__":
    main()
