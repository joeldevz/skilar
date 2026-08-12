# Release holdouts

Public cases under `eval/cases/` are validation cases. Real release holdouts are
provided out of band and copied into this ignored directory only after the control and
candidate bundles, experiment manifest, randomization seed, and gates have been
frozen.

The evaluator records the holdout suite digest and aggregate result. It must not print
or persist unsanitized holdout prompts, fixtures, or traces. A disclosed holdout is
retired or rotated before the next prompt-tuning cycle.
