-- A fact can have more than one source (owner decided 28/9). The owner's example:
--
--   T-0007  "Bao met Minh and Khoa for DD on 25/9/2026"    evidence: Teams invite
--   T-0008  "Bao and Minh agreed how to share data 25/9"  evidence: meeting minutes
--                                                         + T-0007
--
-- The minutes prove the content but carry no date; T-0007 carries the date and the
-- fact that the meeting took place. Two sources for one fact, not a derivation, so the
-- "a derivation needs two premises" rule does not apply: the new content comes from
-- documents, not from inference.
--
-- This is evidence, not a premise, so it has its own column instead of using
-- `premises`. But if it falls, the claim must also read as needs_review: once the thing
-- that gives the claim its date collapses, that date has nothing left to stand on.

ALTER TABLE truth_ledger
  ADD COLUMN IF NOT EXISTS source_entry text;
