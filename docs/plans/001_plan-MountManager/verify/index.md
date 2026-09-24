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
| V001-F1 | MINOR | Context-cancel test asserts "no down" with `&&` | OPEN | C1 | recovery_test.go:277 |
| V001-F2 | MINOR | TestParseMalformed misses separator branch; escapes partially tested | OPEN | C1 | mountinfo_test.go:71-82 |
| V001-F3 | MINOR | Contract behaviors without unit tests | OPEN | C1 | report §F3 list |
| V001-F4 | MINOR | README / example config inaccuracies | OPEN | C2 | README §6 payload, command_grace |
| V001-F5 | MINOR | Stale state/plan metadata | OPEN (STATE row fixed at audit closure; phase_04 header pending C2) | C2 | STATE §7 row; phase_04.md:14 |

Statuses: OPEN, FIXED, ACCEPTED (user decision), OBSOLETE (reason recorded).
Never silently remove a finding. Update the source report's resolution too.

## Open blockers
none
