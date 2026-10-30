# Data quality: profiles, drift, leakage and releases

Data problems are usually found by people, weeks after they start. This makes the platform find the common ones when a snapshot is taken: columns that moved, a reprocessing that mixed two baselines in one snapshot, evaluation items that are also in training, and patients split across train and test. Each check says what it did not look at. Migration `000039_quality.sql`.

## Column profiles

Every new version (snapshot, merge or derivation) is queued for profiling by a database trigger, and the dataset worker profiles it: each table's rows, and per column its kind (`number`, `string`, `bool`, `time`, `nested`, `mixed`, `empty`), values and nulls, distinct values (exact up to 10,000), minimum, maximum, mean and standard deviation, a 2,000-value reservoir sample, the 20 most common values and string lengths. The sample is seeded by the version, so profiling the same bytes twice gives the same profile.

Tables are CSV, TSV, JSON lines (each also gzipped) and Parquet, recognised by extension or content type. A table is a folder of parts (`scenes/part-*.csv` is the table `scenes`) or a file at the root. Members are read through their storage connection, from their pack entry when packed, pinned to the version the snapshot recorded.

`GET /api/v1/datasets/{id}/versions/{versionID}/profile` (`datasets.read`) returns the profile: `state` (`PENDING`, `RUNNING`, `DONE`, `FAILED`, or `NONE` when the dataset does not profile), the tables, `other_files` (members that are not tables), and `skipped`: every table file not profiled, with the reason (erased, larger than 4 GiB, past the version's 50 GiB budget, a table past 50 million rows, unreadable, not valid CSV or Parquet). `POST` profiles it again (`datasets.manage`).

## Drift

When a profile is done it is compared with its baseline: the version's parent, or the version before it. For each table both sides have: the row count's change, columns added and removed, and per column the kind, the null rate, and the population stability index (PSI) of the values: numbers over the baseline sample's deciles, strings and booleans over the values either side lists. Columns whose values are nearly all distinct (identifiers) are not compared, and say so. A breach is a row change, null-rate change or PSI past the dataset's thresholds, a kind change, a removed column, or a table that disappeared. Breaches send `dataset.drift_detected`.

`GET .../versions/{versionID}/drift[?against=VERSION]` compares any two profiled versions. Thresholds are per dataset:

`GET/PUT /api/v1/datasets/{id}/quality-settings` (`datasets.manage` to change): `profile_snapshots` (true), `max_psi` (0.2), `max_null_change` (0.1, ten points), `max_row_change` (0.5, half), and `subject_key` or `subject_pattern` (below).

## Readiness rules

Three rule types for validation profiles:

| Type | Config | Finds |
|---|---|---|
| `column_drift` | optional `max_psi`, `max_null_change`, `max_row_change` | `COLUMN_DRIFT`, `TABLE_DRIFT` against the baseline. Not assessed until the profile is done. |
| `uniform_metadata` | `key`, `require_presence` | `MIXED_METADATA` when members carry more than one value of the key: a processing baseline (Sentinel-2's 04.00 and 05.00), a scanner model, a software version. |
| `subject_leakage` | `key` or `pattern` | `SUBJECT_LEAKAGE`: one patient, user or scene in more than one split. `pattern` is a regular expression whose first group is the subject, as in `^patients/([^/]+)/`. |

## Leakage between training and evaluation

`POST /api/v1/datasets/{id}/versions/{versionID}/leakage {"train_version_id"}` (`datasets.read`) compares the evaluation version in the path with a training version:

- `EXACT_HASH`: evaluation files with the same SHA-256 as a training file (files without a checksum are counted and not compared);
- `SUBJECT`: evaluation files whose subject (the evaluation dataset's `subject_key` metadata, or `subject_pattern` on the path) also has files in training; `NOT_CHECKED` when no subject is set.

The answer lists both checks with up to 20 examples each, and `not_checked`: subject leakage when no subject is set, and near-duplicate contamination unless an n-gram check was recorded for the pair. Exact hashes find copies, not paraphrases.

A training run records what it used each version for: `POST /api/v1/training-runs {..., "roles": {"VERSION": "TRAIN" | "VALIDATION" | "TEST"}}`. The dataset worker compares every evaluation input of such a run with every training input; leakage sends `training_run.leakage_detected`. `GET /api/v1/training-runs/{id}/leakage` lists the run's checks; `POST` runs them now.

Text contamination is compared where the text is: `osp leakage EVAL_DATASET EVAL_VERSION TRAIN_DATASET TRAIN_VERSION --ngram 13` reads both versions' text files, finds evaluation texts that share at least the threshold of their n-grams with training, and records the result with `POST /api/v1/leakage-checks` (`datasets.write`), as the caller's.

## Releases

`PUT /api/v1/datasets/{id}/versions/{versionID}/release {"label", "notes"}` (`datasets.manage`) names a version a release, once: `DR1`, `2026.10`, `v3` (1 to 40 letters, digits, `.`, `_`, `-`, unique in the dataset). `GET /api/v1/datasets/{id}/releases` lists them in order. `GET /api/v1/datasets/{id}/releases/{label}/diff[?against=LABEL]` compares a release with the one before it: files added, removed, changed and relabelled, the first 50 of them, and the drift between their profiles.
