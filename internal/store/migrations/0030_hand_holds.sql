-- Holds of auto-approval that a comment or an approval by hand made, which no longer hold: since 2026-11-03 only a
-- changes request by hand, a dismissed review of the operator's (unless GitHub dismissed it on a push) and a dismissal
-- of one of magnum's approvals stop auto-approval. Their rows go, so the next pass reads the PR's reviews from the
-- start, and a changes request the operator posted before the comment or the approval holds it again. The other
-- reasons and a resumed row (held = 0) stay. Applied when PRAGMA user_version < 30. Only deletes rows: no table is
-- rewritten.

DELETE FROM auto_approve_holds
WHERE held = 1 AND (reason LIKE 'you commented on it by hand%' OR reason LIKE 'you approved it by hand%');
