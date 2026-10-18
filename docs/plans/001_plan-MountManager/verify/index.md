# MountManager — Audit register

## Historical reports
| ID | File | Date | Original verdict | Original counts | Auditor |
|----|------|------|------------------|-----------------|---------|
| V001 | [verify_001_2026-09-24.md](verify_001_2026-09-24.md) | 2026-09-24 | PASS WITH FINDINGS | 0 BLOCKER, 0 MAJOR, 5 MINOR | agent-1:opus + independent read-only reviewer |

Verdicts: PASS, PASS WITH FINDINGS, FAIL, INCOMPLETE. Historical counts/verdicts
are not recomputed as findings are fixed; current unresolved findings are below.

## Findings
| ID | Severity | Title | Current status | Closed by / date | Evidence |
|----|----------|-------|----------------|------------------|----------|
| V001-F1 | MINOR | Context-cancel test asserts "no down" with `&&` | FIXED | C1 / 2026-10-18 | `TestRecoverContextCanceled` now uses `\|\|` and also rejects kill/detach |
| V001-F2 | MINOR | TestParseMalformed misses separator branch; escapes partially tested | FIXED | C1 / 2026-10-18 | `TestParseMalformed` (separator), `TestParseTooFewFields`, `TestParseBasic` 3 lines, `TestUnescapeAll` |
| V001-F3 | MINOR | Contract behaviors without unit tests | FIXED | C1 / 2026-10-18 | `TestCleanupAbortErrorContinues`, `TestCleanupDetachErrorReturned`, `TestEventFields`, `TestValidateErrors` (+11 cases), `TestExecPathDefault`, `TestLogLevelValue`, `TestRunErrorTruncatesStderr`, `TestEvaluateExecSkippedWhenProbeFails`, `TestKillAndReapAbandonsStuckProcess`; `go test -race ./...` green |
| V001-F4 | MINOR | README / example config inaccuracies | FIXED | C2 / 2026-10-18 | README §6 payload `attempt: 1` + retry detail sentence; §4 and config.example.yml command_grace wording |
| V001-F5 | MINOR | Stale state/plan metadata | FIXED | C2 / 2026-10-18 | phase_04.md header "Plan revision: 2"; STATE row fixed at audit closure |

Statuses: OPEN, FIXED, ACCEPTED (user decision), OBSOLETE (reason recorded).
Never silently remove a finding. Update the source report's resolution too.

## Open blockers
none
