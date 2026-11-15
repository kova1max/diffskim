"""Summarizes bench/results.tsv into bench/summary.txt (the numbers the README quotes)."""

import csv
import statistics
import sys
from pathlib import Path

results = Path(sys.argv[1])
rows = list(csv.DictReader(results.open(), delimiter="\t"))
raw = sum(int(r["raw"]) for r in rows)
baseline = sum(int(r["baseline"]) for r in rows)
kept = sum(int(r["kept"]) for r in rows)
cuts = [
    (int(r["baseline"]) - int(r["kept"])) / int(r["baseline"])
    for r in rows
    if int(r["baseline"]) > 0
]
gate = sum(1 for c in cuts if c >= 0.30)
rules = ["lockfile", "generated", "whitespace-only", "import-only", "format-only"]

lines = [
    f"PRs: {len(rows)}",
    f"changed lines, all PRs: raw {raw}, git diff -M -w + lockfile excludes {baseline}, diffskim {kept}",
    f"diffskim vs baseline, all PRs together: {(baseline - kept) / baseline:.1%} fewer lines",
    f"diffskim vs baseline, median PR: {statistics.median(cuts):.1%} fewer lines",
    f"PRs where diffskim keeps at least 30% fewer lines than the baseline: {gate} of {len(cuts)}",
    f"PRs where diffskim keeps more lines than the baseline: {sum(1 for c in cuts if c < 0)} of {len(cuts)}",
    "folded lines by rule: "
    + ", ".join(f"{r} {sum(int(x[r]) for x in rows)}" for r in rules),
]
summary = "\n".join(lines) + "\n"
(results.parent / "summary.txt").write_text(summary)
print(summary, end="")
