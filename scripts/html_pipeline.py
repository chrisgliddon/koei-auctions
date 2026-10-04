#!/usr/bin/env python3
"""
HTML fixture pipeline: watch folder -> GitHub (koei-auctions, public)
-> Airtable HTML field attach. Zero approval cards (gh is pre-authenticated).

File naming (enforced): <source>-<auction-id>-<YYYY-MM-DD>.html
  e.g. surugaya-140001202-2026-10-04.html
Watch folder layout: <watch>/<source>/<file>

For each new file:
  1. Validate it is real listing HTML (reject Cloudflare challenge pages,
     login walls, empty files)
  2. Copy to ~/workspace/koei-auctions/html/<source>/<file>
     (never overwrite an existing file)
  3. git commit + push
  4. PATCH Airtable Auctions HTML field (fldylSRC5pE4trUGU):
     append {"url": raw_url} — never duplicating an existing attachment
  5. Record in the html log

Usage: html_pipeline.py --watch DIR
"""
import json, os, sys, re, shutil, subprocess, time, argparse, urllib.parse

HF = "/home/hatch/workspace/goals/koei-watchlist-monitoring/hidden_files"
REPO = os.path.expanduser("~/workspace/koei-auctions")
HTML_FLD = "fldylSRC5pE4trUGU"
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from airtable_auth import api

BASE_ID = "appMtKWq2FbIe2HTn"
TABLE_ID = "tbl0jGTbqhC6sRFhW"
RAW_BASE = "https://raw.githubusercontent.com/chrisgliddon/koei-auctions/main/html"
FNAME_RE = re.compile(r"^(yahoo|mercari|surugaya)-([A-Za-z0-9]+)-(\d{4}-\d{2}-\d{2})\.html$")
# markers that mean "this is NOT a real listing page"
BAD_MARKERS = ["Just a moment...", "challenges.cloudflare.com",
               "Verify you are human", "Checking your browser"]


def is_valid_html(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            head = f.read(20000)
    except OSError:
        return False
    if len(head.strip()) < 500:
        return False
    return not any(m in head for m in BAD_MARKERS)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--watch", required=True)
    args = ap.parse_args()

    sid2rec = json.load(open(f"{HF}/surugaya_sid_to_record_2026-10-03.json"))
    # yahoo/mercari ids map via UUID too; build reverse map for all sources
    log_path = f"{HF}/html_attach_log.json"
    log = json.load(open(log_path)) if os.path.exists(log_path) else {"attached": {}}

    pending = []
    for source in ("yahoo", "mercari", "surugaya"):
        d = os.path.join(args.watch, source)
        if not os.path.isdir(d):
            continue
        for fn in sorted(os.listdir(d)):
            m = FNAME_RE.match(fn)
            if not m:
                continue
            src, aid = m.group(1), m.group(2)
            key = f"{src}:{aid}:{m.group(3)}"
            if key in log["attached"]:
                continue
            pending.append((src, aid, m.group(3), os.path.join(d, fn), key))
    if not pending:
        print("nothing new")
        return

    # resolve record ids via UUID (surugaya map covers surugaya; others via API lookup)
    import uuid as uuidmod
    ns = uuidmod.UUID(open(f"{HF}/uuid_namespace.txt").read().strip())
    to_push = []
    for src, aid, date, path, key in pending:
        if not is_valid_html(path):
            print(f"SKIP (not valid listing HTML): {path}")
            continue
        if src == "surugaya":
            rid = sid2rec.get(aid)
        else:
            uid = str(uuidmod.uuid5(ns, f"{src}:{aid.strip().lower()}"))
            res = api("GET", f"{BASE_ID}/{TABLE_ID}?filterByFormula=" +
                      urllib.parse.quote("{Listing UUID}='" + uid + "'") +
                      "&fields%5B%5D=HTML")
            recs = res.get("records", [])
            rid = recs[0]["id"] if recs else None
        if not rid:
            print(f"SKIP (no record): {src} {aid}")
            continue
        to_push.append((src, aid, date, path, key, rid))

    for src, aid, date, path, key, rid in to_push:
        dst_dir = os.path.join(REPO, "html", src)
        os.makedirs(dst_dir, exist_ok=True)
        dst = os.path.join(dst_dir, os.path.basename(path))
        if not os.path.exists(dst):
            shutil.copy2(path, dst)
    subprocess.run(["git", "add", "html/"], cwd=REPO, check=True, capture_output=True)
    subprocess.run(
        ["git", "-c", "user.name=Obi",
         "-c", "user.email=chrisgliddon@users.noreply.github.com",
         "commit", "-m", f"html fixtures: {len(to_push)} pages", "--quiet"],
        cwd=REPO, check=True, capture_output=True)
    subprocess.run(["git", "push", "origin", "main"], cwd=REPO, check=True,
                   capture_output=True, timeout=120)
    print(f"pushed {len(to_push)} html files")

    for src, aid, date, path, key, rid in to_push:
        fn = os.path.basename(path)
        url = f"{RAW_BASE}/{src}/{fn}"
        # append without duplicating: read current, add if url not present
        rec = api("GET", f"{BASE_ID}/{TABLE_ID}/{rid}?fields%5B%5D=HTML")
        cur = rec.get("fields", {}).get("HTML", [])
        if any(a.get("url") == url for a in cur):
            print(f"already attached: {key}")
        else:
            api("PATCH", f"{BASE_ID}/{TABLE_ID}",
                {"records": [{"id": rid,
                              "fields": {"HTML": cur + [{"url": url, "filename": fn}]}}],
                 "typecast": True})
            print(f"attached: {key}")
            time.sleep(0.4)
        log["attached"][key] = {"record_id": rid, "url": url,
                                "ts": time.strftime("%Y-%m-%dT%H:%M")}
        json.dump(log, open(log_path + ".tmp", "w"), indent=1)
        os.replace(log_path + ".tmp", log_path)
    print("done")

if __name__ == "__main__":
    main()
