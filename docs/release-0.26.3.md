# Release 0.26.3 — connector checkpoint recovery, wider background budget

Status: released.

## A shorter page no longer fails the run

A GitHub checkpoint stores the page and the offset inside it. When the provider
returned a shorter page than the checkpoint expected — results shifted, or the
run used a different `--since` window — the source failed with
`github.checkpoint: offset exceeds provider page`. It now re-reads that page
from the start; items are content-addressed, so a re-read cannot duplicate
them.

## Background connector budget

The live GitHub connection has 17 repositories and re-indexed 4,023 items after
the raw-zone scrub; the run finished at the 15-minute ceiling. The background
budget is now 30 minutes so a cold start cannot clip the run. Later runs are
incremental and take seconds.

## Verified

`brain connection run github --days 1` finished with exit 0 on the live data
(4,023 items indexed), and the daily `Company Brain Graph Refresh` task ran
through `brain-task.exe` with no console window and exit 0.
