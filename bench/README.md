# Benchmark

`bench/run.sh` measures how many changed lines are left to read in each pull request in `prs.tsv`:

| Column | Command | Counted by |
|---|---|---|
| `raw` | `git diff -M base head` | `git diff --numstat` |
| `baseline` | `git diff -M -w base head -- . ':(exclude,glob)**/<lockfile>'...`, for every name `diffskim --lockfiles` prints | `git diff --numstat` |
| `kept` | `diffskim base head` | `git apply --numstat` on diffskim's output |

`base` is the merge base of the pull request and `head` its last commit, so the diff is the pull request's diff. The script also checks that every diffskim output passes `git apply --check` against `base`, and records the lines each rule folded. `summary.txt` holds the totals that the main README quotes.

```sh
bench/run.sh    # needs git, Go, Python 3, and network access the first time
```

Repositories are cached as blobless partial clones in `~/.cache/diffskim-bench` (set `DIFFSKIM_BENCH_CACHE` to move it). A rerun with a warm cache took about a minute on a laptop.

## How the pull requests were picked

On 2026-11-15, these GitHub searches were run (the first 300 results each):

```text
is:pr is:merged author:app/copilot-swe-agent language:typescript prettier
is:pr is:merged author:app/copilot-swe-agent language:typescript format
is:pr is:merged author:app/copilot-swe-agent language:typescript lint
is:pr is:merged author:app/copilot-swe-agent language:typescript refactor
is:pr is:merged "Generated with Claude Code" language:typescript refactor
is:pr is:merged author:app/devin-ai-integration language:typescript refactor
is:pr is:merged author:app/copilot-swe-agent language:javascript refactor
is:pr is:merged author:app/copilot-swe-agent language:typescript migrate
```

`prs.tsv` is every result in a repository with at least 1,000 stars and 150 to 5,000 changed lines. None was added or removed by hand. The keywords lean toward pull requests with formatting and import churn, which favors diffskim. Even so, most pull requests in the set are ordinary feature work.
