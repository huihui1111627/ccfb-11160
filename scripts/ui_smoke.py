#!/usr/bin/env python3
"""Headless smoke test for the dashboard.

Usage:
    python3 scripts/ui_smoke.py http://127.0.0.1:8080

Requires playwright (pip install playwright && playwright install chromium).
It walks all five tabs against a running bankcmp instance that already has at
least one finished task, and exits non-zero on a JavaScript error.
"""
import sys
from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"
failures = []


def main() -> int:
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1440, "height": 1000})
        page.on("pageerror", lambda e: failures.append(f"pageerror: {e}"))
        page.on("console", lambda m: failures.append(f"console.error: {m.text}")
                if m.type == "error" else None)
        page.goto(BASE + "/", wait_until="networkidle")

        page.wait_for_selector("#conn-list .item")
        print(f"connections: {page.locator('#conn-list .item').count()}")

        page.click('.tab[data-tab="tasks"]')
        page.wait_for_selector("#task-list .item")
        page.click("#task-list .item button:has-text('详情')")
        page.wait_for_selector("#table-runs tbody tr")
        print(f"tables: {page.locator('#table-runs tbody tr').count()}")

        page.click('.tab[data-tab="diffs"]')
        page.wait_for_selector("#diff-table-view tbody tr")
        page.wait_for_timeout(400)
        print(f"diffs: {page.locator('#diff-page').inner_text()}")
        page.select_option("#diff-kind", "modified")
        page.wait_for_timeout(500)
        print(f"modified: {page.locator('#diff-table-view tbody tr').count()}")

        page.click('.tab[data-tab="repairs"]')
        page.wait_for_function("document.querySelectorAll('#repair-table option').length > 0")
        page.select_option("#repair-kinds", "missing")
        page.click("#repair-form button[type=submit]")
        page.wait_for_selector("#repair-preview:not(.hidden)")
        sql = page.locator("#repair-sql").inner_text()
        assert sql.strip().startswith("INSERT INTO"), "expected an INSERT preview"
        print(f"repair: {page.locator('#repair-count').inner_text()}")

        page.click('.tab[data-tab="report"]')
        page.wait_for_selector("#report-table tbody tr")
        print(f"report rows: {page.locator('#report-table tbody tr').count()}")
        browser.close()

    if failures:
        print("failures:")
        for f in failures:
            print(" ", f)
        return 1
    print("UI OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
