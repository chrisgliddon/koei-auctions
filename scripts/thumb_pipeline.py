#!/usr/bin/env python3
"""
Surugaya thumbnail pipeline: watch folder -> GitHub (koei-auctions, public)
-> Airtable Images attach. Zero approval cards (gh is pre-authenticated).

Usage:
  thumb_pipeline.py --watch DIR     Process new .jpg files in DIR (named <listing-id>.jpg)
  thumb_pipeline.py --watch DIR --daemon  (not implemented; run via cron every N min)

For each new file:
  1. Copy to ~/workspace/koei-auctions/thumbs/<listing-id>.jpg
  2. git commit + push (public repo -> raw.githubusercontent.com URLs work)
  3. PATCH Airtable Auctions: Images=[{"url": raw_url}], Image Status="Has images"
  4. Record in the attach log (skip if already uploaded)

Requires: ~/workspace/koei-auctions cloned, gh authenticated, sid->record map.
"""
import json, os, sys, shutil, subprocess, time, argparse

HF = "/home/hatch/workspace/goals/koei-watchlist-monitoring/hidden_files"
REPO = os.path.expanduser("~/workspace/koei-auctions")
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from airtable_auth import api

BASE_ID = "appMtKWq2FbIe2HTn"
TABLE_ID = "tbl0jGTbqhC6sRFhW"
RAW_BASE = "https://raw.githubusercontent.com/chrisgliddon/koei-auctions/main/thumbs"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--watch", required=True, help="folder to scan for <listing-id>.jpg files")
    args = ap.parse_args()
    watch = os.path.abspath(args.watch)

    sid2rec = json.load(open(f"{HF}/surugaya_sid_to_record_2026-10-03.json"))
    map_path = f"{HF}/surugaya_thumb_attach_2026-10-03.json"
    state = json.load(open(map_path))
    uploaded = state.setdefault("uploaded", {})

    def save():
        tmp = map_path + ".tmp"
        json.dump(state, open(tmp, "w"), indent=1)
        os.replace(tmp, map_path)

    new_files = []
    for fn in sorted(os.listdir(watch)):
        if not fn.endswith(".jpg"):
            continue
        sid = fn[:-4]
        if sid in uploaded or sid not in sid2rec:
            continue
        new_files.append((sid, os.path.join(watch, fn)))
    if not new_files:
        print("nothing new")
        return

    print(f"processing {len(new_files)} new file(s)")
    pushed = []
    for sid, src in new_files:
        dst = os.path.join(REPO, "thumbs", f"{sid}.jpg")
        shutil.copy2(src, dst)
        pushed.append(sid)
    subprocess.run(["git", "add", "thumbs/"], cwd=REPO, check=True,
                   capture_output=True)
    subprocess.run(
        ["git", "-c", "user.name=Obi",
         "-c", "user.email=chrisgliddon@users.noreply.github.com",
         "commit", "-m", f"thumbnails: {', '.join(pushed)}", "--quiet"],
        cwd=REPO, check=True, capture_output=True)
    subprocess.run(["git", "push", "origin", "main"], cwd=REPO, check=True,
                   capture_output=True, timeout=120)
    print(f"pushed {len(pushed)} to koei-auctions")

    recs = [{"id": sid2rec[s], "fields": {
        "Images": [{"url": f"{RAW_BASE}/{s}.jpg", "filename": f"surugaya-{s}.jpg"}],
        "Image Status": "Has images"}} for s in pushed]
    ok = 0
    for j in range(0, len(recs), 10):
        r = api("PATCH", f"{BASE_ID}/{TABLE_ID}",
                {"records": recs[j:j+10], "typecast": True})
        ok += len(r.get("records", []))
        time.sleep(0.5)
    print(f"attached {ok}/{len(pushed)}")
    for s in pushed:
        uploaded[s] = {"record_id": sid2rec[s], "via": "github_koei-auctions",
                       "url": f"{RAW_BASE}/{s}.jpg",
                       "ts": time.strftime("%Y-%m-%dT%H:%M")}
    save()
    print("done")

if __name__ == "__main__":
    main()
